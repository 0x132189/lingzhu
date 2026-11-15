package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/pty"
)

func ptyBody() string {
	if runtime.GOOS == "windows" {
		return `{"command":"cmd.exe","args":["/Q"],"title":"test terminal"}`
	}
	return `{"command":"/bin/sh","title":"test terminal"}`
}

func ptyNewline() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}

func TestPTYEndpoints(t *testing.T) {
	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server := newTestServer(t)
	server.SetPTY(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pty", strings.NewReader(ptyBody())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		PTY pty.Info `json:"pty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.PTY.ID == "" || created.PTY.Status != "running" || created.PTY.Title != "test terminal" {
		t.Fatalf("unexpected pty: %+v", created.PTY)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pty", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.PTY.ID) {
		t.Fatalf("unexpected list: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pty/"+created.PTY.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "test terminal") {
		t.Fatalf("unexpected get: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/pty/"+created.PTY.ID, strings.NewReader(`{"title":"renamed","size":{"rows":30,"cols":100}}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "renamed") {
		t.Fatalf("unexpected update: %d %s", rec.Code, rec.Body.String())
	}

	input, _ := json.Marshal(map[string]string{"data": "echo SRV_PTY" + ptyNewline()})
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pty/"+created.PTY.ID+"/input", strings.NewReader(string(input))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected input status: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var output strings.Builder
	var after int64
	for time.Now().Before(deadline) {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/pty/%s/output?after=%d", created.PTY.ID, after), nil))
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
		if strings.Contains(output.String(), "SRV_PTY") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "SRV_PTY") {
		t.Fatalf("output marker missing: %q", output.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/pty/"+created.PTY.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pty/"+created.PTY.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted pty should 404: %d", rec.Code)
	}
}

func TestShellEndpoints(t *testing.T) {
	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server := newTestServer(t)
	server.SetPTY(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/shell", strings.NewReader(`{"command":"echo SRV_SHELL","metadata":{"session":"s1"}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Shell pty.ShellInfo `json:"shell"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Shell.ID, "sh_") || created.Shell.File == "" {
		t.Fatalf("unexpected shell: %+v", created.Shell)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/shell/"+created.Shell.ID, nil))
		var one struct {
			Shell pty.ShellInfo `json:"shell"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
			t.Fatal(err)
		}
		if one.Shell.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell did not exit: %+v", one.Shell)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/shell/"+created.Shell.ID+"/output?cursor=0&limit=1024", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected output status: %d %s", rec.Code, rec.Body.String())
	}
	var output pty.Output
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, "SRV_SHELL") || output.Cursor > output.Size {
		t.Fatalf("unexpected output: %+v", output)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/shell/"+created.Shell.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/shell/"+created.Shell.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted shell should 404: %d", rec.Code)
	}
}

func TestPTYDisabledAndMissing(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pty", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("disabled pty should 501: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/shell", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("disabled shell should 501: %d", rec.Code)
	}

	manager, err := pty.New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	server.SetPTY(manager)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pty/pty_missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing pty should 404: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/shell/sh_missing/output", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing shell should 404: %d", rec.Code)
	}
}
