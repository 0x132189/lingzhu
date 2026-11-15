package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/model"
)

func postFormJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFormEndpoints(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	created := postFormJSON(t, httpSrv.URL+"/sessions/s1/form", `{
		"title":"Deploy",
		"fields":[
			{"name":"reason","required":true},
			{"name":"scope","type":"select","options":["all","one"]}
		]
	}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 for create form, got %d", created.StatusCode)
	}
	var form formRecord
	if err := json.NewDecoder(created.Body).Decode(&form); err != nil {
		t.Fatal(err)
	}
	if form.ID == "" || form.Title != "Deploy" || form.Status != "pending" || len(form.Fields) != 2 {
		t.Fatalf("unexpected created form: %+v", form)
	}
	if form.Fields[0].Label != "reason" || form.Fields[0].Type != "text" {
		t.Fatalf("field defaults missing: %+v", form.Fields[0])
	}
	if len(form.Fields[1].Options) != 2 {
		t.Fatalf("select options missing: %+v", form.Fields[1])
	}

	listed, err := http.Get(httpSrv.URL + "/sessions/s1/form?status=pending")
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Forms []formRecord `json:"forms"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(pending.Forms) != 1 || pending.Forms[0].ID != form.ID {
		t.Fatalf("unexpected pending forms: %+v", pending.Forms)
	}

	missingRequired := postFormJSON(t, httpSrv.URL+"/sessions/s1/form/"+form.ID, `{"values":{"scope":"all"}}`)
	missingRequired.Body.Close()
	if missingRequired.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing required value should 400, got %d", missingRequired.StatusCode)
	}

	answered := postFormJSON(t, httpSrv.URL+"/sessions/s1/form/"+form.ID, `{"values":{"reason":"because","scope":"all"}}`)
	defer answered.Body.Close()
	if answered.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for form reply, got %d", answered.StatusCode)
	}
	var replied formRecord
	if err := json.NewDecoder(answered.Body).Decode(&replied); err != nil {
		t.Fatal(err)
	}
	if replied.Status != "answered" || replied.Values["reason"] != "because" || replied.AnsweredAt == nil {
		t.Fatalf("unexpected answered form: %+v", replied)
	}

	duplicate := postFormJSON(t, httpSrv.URL+"/sessions/s1/form/"+form.ID, `{"values":{"reason":"again"}}`)
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate reply should 409, got %d", duplicate.StatusCode)
	}

	listed, err = http.Get(httpSrv.URL + "/sessions/s1/form?status=pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(listed.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(pending.Forms) != 0 {
		t.Fatalf("answered form should not be pending: %+v", pending.Forms)
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(messages) < 3 {
		t.Fatalf("form answer was not delivered to the session: %+v", messages)
	}
	if !messages[1].Synthetic || !strings.Contains(messages[1].Content, "Deploy") || !strings.Contains(messages[1].Content, "because") {
		t.Fatalf("unexpected form answer message: %+v", messages[1])
	}
}

func TestFormCancellationAndValidation(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	empty := postFormJSON(t, httpSrv.URL+"/sessions/s1/form", `{"title":"Empty","fields":[]}`)
	empty.Body.Close()
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty fields should 400, got %d", empty.StatusCode)
	}

	duplicate := postFormJSON(t, httpSrv.URL+"/sessions/s1/form", `{"fields":[{"name":"a"},{"name":"a"}]}`)
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate field should 400, got %d", duplicate.StatusCode)
	}

	created := postFormJSON(t, httpSrv.URL+"/sessions/s1/form", `{"title":"Confirm","fields":[{"name":"ok","required":true}]}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 for create form, got %d", created.StatusCode)
	}
	var form formRecord
	if err := json.NewDecoder(created.Body).Decode(&form); err != nil {
		t.Fatal(err)
	}

	cancelled := postFormJSON(t, httpSrv.URL+"/sessions/s1/form/"+form.ID, `{"cancelled":true}`)
	defer cancelled.Body.Close()
	if cancelled.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for cancel, got %d", cancelled.StatusCode)
	}
	var replied formRecord
	if err := json.NewDecoder(cancelled.Body).Decode(&replied); err != nil {
		t.Fatal(err)
	}
	if replied.Status != "cancelled" || !replied.Cancelled {
		t.Fatalf("unexpected cancelled form: %+v", replied)
	}

	unknown := postFormJSON(t, httpSrv.URL+"/sessions/s1/form/form_missing", `{"values":{"ok":"yes"}}`)
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown form should 404, got %d", unknown.StatusCode)
	}

	missingSession := postFormJSON(t, httpSrv.URL+"/sessions/missing/form", `{"fields":[{"name":"a"}]}`)
	missingSession.Body.Close()
	if missingSession.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", missingSession.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		var err error
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(messages) < 2 || !strings.Contains(messages[1].Content, "[Form cancelled]") {
		t.Fatalf("cancellation was not delivered to the session: %+v", messages)
	}
}
