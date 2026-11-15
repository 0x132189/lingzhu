package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fileListPayload struct {
	Path      string    `json:"path"`
	Entries   []fsEntry `json:"entries"`
	Truncated bool      `json:"truncated"`
}

type fileContentPayload struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Size      int64  `json:"size"`
}

func newFileServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("ALPHA_CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "beta.txt"), []byte("BETA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("HIDDEN"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", maxFileBytes+100)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newSessionServer(t, func(s *Server) { s.SetWorkdir(root) })
	return srv, root
}

func getFileJSON(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func putFileJSON(t *testing.T, srv *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, target, strings.NewReader(body)))
	return rec
}

func deleteFileJSON(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, target, nil))
	return rec
}

func moveFileJSON(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/file/move", strings.NewReader(body)))
	return rec
}

func createEntryJSON(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/files", strings.NewReader(body)))
	return rec
}

func TestCreateEntry(t *testing.T) {
	srv, root := newFileServer(t)

	rec := createEntryJSON(t, srv, `{"path":"newdir","type":"dir"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 for directory, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int    `json:"size"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Path != "newdir" || created.Type != "dir" || created.Size != 0 {
		t.Fatalf("unexpected directory result: %+v", created)
	}
	if info, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !info.IsDir() {
		t.Fatalf("directory missing: %v", err)
	}

	rec = createEntryJSON(t, srv, `{"path":"newdir/new.txt","type":"file","content":"NEW_CONTENT"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 for file, got %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "newdir", "new.txt"))
	if err != nil || string(data) != "NEW_CONTENT" {
		t.Fatalf("file missing: %q %v", data, err)
	}

	rec = createEntryJSON(t, srv, `{"path":"newdir/empty.txt"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("default type should create a file, got %d", rec.Code)
	}

	rec = createEntryJSON(t, srv, `{"path":"alpha.txt","type":"file"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("existing path should 409, got %d", rec.Code)
	}

	rec = createEntryJSON(t, srv, `{"path":"","type":"dir"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty path should 400, got %d", rec.Code)
	}

	rec = createEntryJSON(t, srv, `{"path":"../outside","type":"dir"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal should 400, got %d", rec.Code)
	}

	rec = createEntryJSON(t, srv, `{"path":"bogus","type":"link"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown type should 400, got %d", rec.Code)
	}

	rec = createEntryJSON(t, srv, `{"path":"missing/new.txt","type":"file"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing parent should 404, got %d", rec.Code)
	}

	big := strings.Repeat("x", maxWriteBytes+1)
	rec = createEntryJSON(t, srv, `{"path":"big-create.txt","type":"file","content":"`+big+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized content should 413, got %d", rec.Code)
	}
}

func TestDeleteAndMoveFile(t *testing.T) {
	srv, root := newFileServer(t)

	rec := deleteFileJSON(t, srv, "/file?path=alpha.txt")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204 for delete, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be removed: %v", err)
	}

	rec = deleteFileJSON(t, srv, "/file?path=sub")
	if rec.Code != http.StatusConflict {
		t.Fatalf("non-empty directory should 409, got %d", rec.Code)
	}

	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	rec = deleteFileJSON(t, srv, "/file?path=empty")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("empty directory should be removable, got %d", rec.Code)
	}

	rec = deleteFileJSON(t, srv, "/file?path=")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("workspace root should 400, got %d", rec.Code)
	}

	rec = deleteFileJSON(t, srv, "/file?path=..%2Foutside.txt")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal delete should 400, got %d", rec.Code)
	}

	rec = moveFileJSON(t, srv, `{"from":"binary.bin","to":"sub/moved.bin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for move, got %d: %s", rec.Code, rec.Body.String())
	}
	var moved struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&moved); err != nil {
		t.Fatal(err)
	}
	if moved.From != "binary.bin" || moved.To != "sub/moved.bin" {
		t.Fatalf("unexpected move result: %+v", moved)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "moved.bin")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}

	rec = moveFileJSON(t, srv, `{"from":"big.txt","to":"sub/moved.bin"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("existing destination should 409, got %d", rec.Code)
	}

	rec = moveFileJSON(t, srv, `{"from":"missing.bin","to":"next.bin"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing source should 404, got %d", rec.Code)
	}

	rec = moveFileJSON(t, srv, `{"from":"sub/moved.bin","to":"no/such/file.bin"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing destination parent should 404, got %d", rec.Code)
	}

	rec = moveFileJSON(t, srv, `{"from":"sub/moved.bin","to":"../outside.bin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("outside destination should 400, got %d", rec.Code)
	}
}

func TestWriteFile(t *testing.T) {
	srv, root := newFileServer(t)

	rec := putFileJSON(t, srv, "/file?path=alpha.txt", `{"content":"UPDATED_CONTENT"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for write, got %d: %s", rec.Code, rec.Body.String())
	}
	var written struct {
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&written); err != nil {
		t.Fatal(err)
	}
	if written.Path != "alpha.txt" || written.Size != len("UPDATED_CONTENT") {
		t.Fatalf("unexpected write result: %+v", written)
	}
	data, err := os.ReadFile(filepath.Join(root, "alpha.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "UPDATED_CONTENT" {
		t.Fatalf("file was not replaced: %q", data)
	}

	rec = getFileJSON(t, srv, "/file?path=alpha.txt")
	var preview fileContentPayload
	if err := json.NewDecoder(rec.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if preview.Content != "UPDATED_CONTENT" {
		t.Fatalf("preview did not see the write: %+v", preview)
	}

	rec = putFileJSON(t, srv, "/file?path=sub/new.txt", `{"content":"NEW_FILE"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for new file, got %d", rec.Code)
	}
	if data, err := os.ReadFile(filepath.Join(root, "sub", "new.txt")); err != nil || string(data) != "NEW_FILE" {
		t.Fatalf("new file missing: %q %v", data, err)
	}

	rec = putFileJSON(t, srv, "/file?path=sub", `{"content":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("writing a directory should 400, got %d", rec.Code)
	}

	rec = putFileJSON(t, srv, "/file?path=..%2Foutside.txt", `{"content":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal should 400, got %d", rec.Code)
	}

	rec = putFileJSON(t, srv, "/file?path=missing%2Ffile.txt", `{"content":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing parent should 404, got %d", rec.Code)
	}

	big := strings.Repeat("x", maxWriteBytes+1)
	rec = putFileJSON(t, srv, "/file?path=big-write.txt", `{"content":"`+big+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized content should 413, got %d", rec.Code)
	}
}

func TestFileListingAndPreview(t *testing.T) {
	srv, _ := newFileServer(t)

	rec := getFileJSON(t, srv, "/files")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for listing, got %d: %s", rec.Code, rec.Body.String())
	}
	var listed fileListPayload
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if listed.Path != "" || listed.Truncated {
		t.Fatalf("unexpected listing metadata: %+v", listed)
	}
	names := make([]string, 0, len(listed.Entries))
	for _, entry := range listed.Entries {
		names = append(names, entry.Name)
	}
	want := []string{"sub", "alpha.txt", "big.txt", "binary.bin"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected entries: %v", names)
	}
	if listed.Entries[0].Type != "dir" || listed.Entries[1].Type != "file" {
		t.Fatalf("unexpected entry kinds: %+v", listed.Entries)
	}
	for _, entry := range listed.Entries {
		if entry.Name == ".hidden" {
			t.Fatal("hidden files should be excluded by default")
		}
	}

	rec = getFileJSON(t, srv, "/files?hidden=true")
	var withHidden fileListPayload
	if err := json.NewDecoder(rec.Body).Decode(&withHidden); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range withHidden.Entries {
		if entry.Name == ".hidden" && entry.Hidden {
			found = true
		}
	}
	if !found {
		t.Fatalf("hidden file missing: %+v", withHidden.Entries)
	}

	rec = getFileJSON(t, srv, "/files?path=sub")
	var sub fileListPayload
	if err := json.NewDecoder(rec.Body).Decode(&sub); err != nil {
		t.Fatal(err)
	}
	if sub.Path != "sub" || len(sub.Entries) != 1 || sub.Entries[0].Path != "sub/beta.txt" {
		t.Fatalf("unexpected sub listing: %+v", sub)
	}

	rec = getFileJSON(t, srv, "/file?path=alpha.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for file preview, got %d", rec.Code)
	}
	var preview fileContentPayload
	if err := json.NewDecoder(rec.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if preview.Path != "alpha.txt" || preview.Content != "ALPHA_CONTENT" || preview.Binary || preview.Truncated {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if preview.Size != int64(len("ALPHA_CONTENT")) {
		t.Fatalf("unexpected size: %+v", preview)
	}

	rec = getFileJSON(t, srv, "/file?path=binary.bin")
	var binary fileContentPayload
	if err := json.NewDecoder(rec.Body).Decode(&binary); err != nil {
		t.Fatal(err)
	}
	if !binary.Binary || binary.Content != "" {
		t.Fatalf("binary file should not be rendered: %+v", binary)
	}

	rec = getFileJSON(t, srv, "/file?path=big.txt")
	var big fileContentPayload
	if err := json.NewDecoder(rec.Body).Decode(&big); err != nil {
		t.Fatal(err)
	}
	if !big.Truncated || len(big.Content) != maxFileBytes {
		t.Fatalf("big file should be truncated: %+v (%d bytes)", big, len(big.Content))
	}
}

func TestFileErrors(t *testing.T) {
	srv, root := newFileServer(t)

	outside := url.QueryEscape(filepath.Dir(root))
	rec := getFileJSON(t, srv, "/files?path="+outside)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("parent directory should 400, got %d", rec.Code)
	}

	rec = getFileJSON(t, srv, "/files?path=..%2F")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal should 400, got %d", rec.Code)
	}

	rec = getFileJSON(t, srv, "/file?path=sub")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("directory read should 400, got %d", rec.Code)
	}

	rec = getFileJSON(t, srv, "/file?path=missing.txt")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing file should 404, got %d", rec.Code)
	}

	rec = getFileJSON(t, srv, "/files?path=missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing directory should 404, got %d", rec.Code)
	}
}
