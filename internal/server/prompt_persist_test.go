package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPromptPersistFlag(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")

	run := func(body string) {
		t.Helper()
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()
		resp, err := http.Post(httpSrv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected prompt status: %d %s", resp.StatusCode, data)
		}
	}

	run(`{"text":"one","provider":"test","model":"m","persist":false}`)
	meta, err := store.Meta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "" || meta.Model != "" {
		t.Fatalf("persist=false should not change selection: %+v", meta)
	}

	run(`{"text":"two","provider":"test","model":"m"}`)
	meta, err = store.Meta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "test" || meta.Model != "m" {
		t.Fatalf("default prompt should persist selection: %+v", meta)
	}
}
