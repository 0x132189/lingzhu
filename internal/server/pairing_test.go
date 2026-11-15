package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pairingPayload struct {
	Pairing pairingEntry `json:"pairing"`
}

func pairingPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPairingLifecycle(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	created := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing", `{}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 for pairing, got %d", created.StatusCode)
	}
	var payload pairingPayload
	if err := json.NewDecoder(created.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	entry := payload.Pairing
	if len(entry.Code) != 8 || entry.SessionID != "s1" || entry.ExpiresIn != 60 {
		t.Fatalf("unexpected pairing: %+v", entry)
	}
	if !entry.ExpiresAt.After(time.Now()) {
		t.Fatalf("pairing already expired: %+v", entry)
	}

	listed, err := http.Get(httpSrv.URL + "/sessions/s1/pairing")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		SessionID string         `json:"sessionId"`
		Pairings  []pairingEntry `json:"pairings"`
		Count     int            `json:"count"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if list.Count != 1 || list.Pairings[0].Code != entry.Code {
		t.Fatalf("unexpected pairing list: %+v", list)
	}

	redeemed := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing/"+entry.Code, `{}`)
	defer redeemed.Body.Close()
	if redeemed.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for redeem, got %d", redeemed.StatusCode)
	}
	var result struct {
		Paired    bool   `json:"paired"`
		SessionID string `json:"sessionId"`
		Directory string `json:"directory"`
	}
	if err := json.NewDecoder(redeemed.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Paired || result.SessionID != "s1" {
		t.Fatalf("unexpected redeem result: %+v", result)
	}

	again := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing/"+entry.Code, `{}`)
	again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("codes must be single-use, got %d", again.StatusCode)
	}

	second := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing", `{"ttlSeconds":9999}`)
	defer second.Body.Close()
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 for second pairing, got %d", second.StatusCode)
	}
	if err := json.NewDecoder(second.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Pairing.ExpiresIn != int(maxPairingTTL.Seconds()) {
		t.Fatalf("ttl should clamp to %v: %+v", maxPairingTTL, payload.Pairing)
	}

	req, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/sessions/s1/pairing/"+payload.Pairing.Code, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 for revoke, got %d", resp.StatusCode)
	}

	missing := pairingPost(t, httpSrv.URL+"/sessions/missing/pairing", `{}`)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", missing.StatusCode)
	}

	unknown := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing/deadbeef", `{}`)
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown code should 404, got %d", unknown.StatusCode)
	}
}

func TestPairingExpiry(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	now := time.Now()
	srv.pairings.now = func() time.Time { return now }
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	created := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing", `{"ttlSeconds":30}`)
	defer created.Body.Close()
	var payload pairingPayload
	if err := json.NewDecoder(created.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Pairing.ExpiresIn != 30 {
		t.Fatalf("unexpected ttl: %+v", payload.Pairing)
	}

	now = now.Add(31 * time.Second)
	listed, err := http.Get(httpSrv.URL + "/sessions/s1/pairing")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if list.Count != 0 {
		t.Fatalf("expired codes should be purged: %+v", list)
	}

	redeemed := pairingPost(t, httpSrv.URL+"/sessions/s1/pairing/"+payload.Pairing.Code, `{}`)
	redeemed.Body.Close()
	if redeemed.StatusCode != http.StatusNotFound {
		t.Fatalf("expired code should 404, got %d", redeemed.StatusCode)
	}
}
