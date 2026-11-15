package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/pty"
)

func persistentBody() string {
	if runtime.GOOS == "windows" {
		return `{"command":"cmd.exe","args":["/Q"],"title":"persist","size":{"cols":100,"rows":30}}`
	}
	return `{"command":"/bin/sh","title":"persist","size":{"cols":100,"rows":30}}`
}

func TestPersistentPTYEndpoints(t *testing.T) {
	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server := newTestServer(t)
	server.SetPTY(manager)
	if _, err := server.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/terminal", strings.NewReader(persistentBody())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data pty.PersistentInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Data.ID
	if id == "" || created.Data.SessionID != "s1" || created.Data.Status != "running" || created.Data.Size.Cols != 100 {
		t.Fatalf("unexpected terminal: %+v", created.Data)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/terminal", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("unexpected list: %d %s", rec.Code, rec.Body.String())
	}

	input, _ := json.Marshal(map[string]string{"data": "echo PERSIST_SRV" + ptyNewline()})
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/"+id+"/input", strings.NewReader(string(input))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected input status: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var output strings.Builder
	var after int64
	for time.Now().Before(deadline) {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/output?after="+itoa(after), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected output status: %d %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Chunks []pty.Chunk `json:"chunks"`
			Next   int64       `json:"next"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		after = page.Next
		for _, chunk := range page.Chunks {
			output.WriteString(chunk.Data)
		}
		if strings.Contains(output.String(), "PERSIST_SRV") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "PERSIST_SRV") {
		t.Fatalf("marker missing: %q", output.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/terminal/read?lines=50", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PERSIST_SRV") {
		t.Fatalf("unexpected read: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/snapshot", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PERSIST_SRV") {
		t.Fatalf("unexpected snapshot: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/persistent-pty/"+id, strings.NewReader(`{"title":"renamed","size":{"cols":120,"rows":40}}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "renamed") || !strings.Contains(rec.Body.String(), `"cols":120`) {
		t.Fatalf("unexpected update: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/"+id+"/connect-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected token status: %d %s", rec.Code, rec.Body.String())
	}
	var tokenPayload struct {
		Data struct {
			Ticket string `json:"ticket"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tokenPayload); err != nil {
		t.Fatal(err)
	}
	if tokenPayload.Data.Ticket == "" {
		t.Fatalf("missing ticket: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/connect?ticket=bogus", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid ticket should 403: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/shutdown", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected shutdown status: %d", rec.Code)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		one, found := manager.GetPersistent(id)
		if found && one.Status == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal did not exit: %+v", one)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/connect?ticket="+tokenPayload.Data.Ticket, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected connect status: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: snapshot") || !strings.Contains(rec.Body.String(), "PERSIST_SRV") || !strings.Contains(rec.Body.String(), "event: exit") {
		t.Fatalf("unexpected connect stream: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/handoff", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"terminals"`) || !strings.Contains(rec.Body.String(), "PERSIST_SRV") {
		t.Fatalf("unexpected handoff: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/persistent-pty/"+id, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted terminal should 404: %d", rec.Code)
	}
}

func TestPersistentPTYHandoffImport(t *testing.T) {
	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server := newTestServer(t)
	server.SetPTY(manager)
	if _, err := server.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/terminal", strings.NewReader(persistentBody())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data pty.PersistentInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Data.ID

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/"+id+"/input", strings.NewReader(`{"data":"echo HANDOFF_SRV\r\n"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected input status: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	var output strings.Builder
	var after int64
	for time.Now().Before(deadline) {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/output?after="+itoa(after), nil))
		var page struct {
			Chunks []pty.Chunk `json:"chunks"`
			Next   int64       `json:"next"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		after = page.Next
		for _, chunk := range page.Chunks {
			output.WriteString(chunk.Data)
		}
		if strings.Contains(output.String(), "HANDOFF_SRV") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "HANDOFF_SRV") {
		t.Fatalf("marker missing: %q", output.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/shutdown", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected shutdown status: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/handoff", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected handoff status: %d %s", rec.Code, rec.Body.String())
	}
	var exported struct {
		Handoff pty.Handoff `json:"handoff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Handoff.Terminals) != 1 {
		t.Fatalf("unexpected handoff terminals: %+v", exported.Handoff)
	}
	entry := exported.Handoff.Terminals[0]
	if entry.Info.ID != id || entry.Info.Status != "exited" || !strings.Contains(entry.Output, "HANDOFF_SRV") {
		t.Fatalf("unexpected handoff entry: %+v", entry)
	}

	payload, err := json.Marshal(map[string]any{"handoff": exported.Handoff})
	if err != nil {
		t.Fatal(err)
	}

	manager2, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager2.CloseAll()
	server2 := newTestServer(t)
	server2.SetPTY(manager2)
	if _, err := server2.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	server2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/import", strings.NewReader(string(payload))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected import status: %d %s", rec.Code, rec.Body.String())
	}
	var imported struct {
		Data []pty.PersistentInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported.Data) != 1 || imported.Data[0].ID != id || imported.Data[0].Status != "exited" || imported.Data[0].PID != 0 {
		t.Fatalf("unexpected imported record: %+v", imported.Data)
	}

	rec = httptest.NewRecorder()
	server2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/terminal", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) || !strings.Contains(rec.Body.String(), `"exited"`) {
		t.Fatalf("unexpected list after import: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/"+id+"/snapshot", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "HANDOFF_SRV") {
		t.Fatalf("unexpected snapshot after import: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/persistent-pty/import", strings.NewReader(string(payload))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected second import status: %d %s", rec.Code, rec.Body.String())
	}
	var again struct {
		Data []pty.PersistentInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Data) != 1 || again.Data[0].ID == id {
		t.Fatalf("duplicate import should get a fresh id: %+v", again.Data)
	}
	items := manager2.ListPersistent("s1")
	if len(items) != 2 {
		t.Fatalf("expected two imported records, got %+v", items)
	}
}

func TestPersistentPTYDisabledAndMissingSession(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/terminal", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("disabled terminal should 501: %d", rec.Code)
	}
	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server.SetPTY(manager)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/terminal", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/persistent-pty/pty_missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing terminal should 404: %d", rec.Code)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
