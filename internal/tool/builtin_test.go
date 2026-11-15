package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/job"
	"lingzhu/internal/media"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
)

func newRegistry(t *testing.T, rules permission.Rules) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	registry := NewRegistry()
	for _, tl := range Builtins([]string{"bash", "-lc"}, dir, job.New()) {
		registry.Add(tl)
	}
	return registry, dir
}

func run(t *testing.T, registry *Registry, name, input string, perm *permission.Manager) (string, error) {
	t.Helper()
	return registry.Run(context.Background(), Call{
		Name:      name,
		Input:     []byte(input),
		Perm:      perm,
		SessionID: "test",
		OnAsk:     func(permission.Request) {},
	})
}

func TestWriteThenRead(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})

	if _, err := run(t, registry, "write", `{"path":"sub/a.txt","content":"line1\nline2\n"}`, perm); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, registry, "read", `{"path":"sub/a.txt"}`, perm)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1: line1") || !strings.Contains(out, "2: line2") {
		t.Fatalf("unexpected read output: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "a.txt")); err != nil {
		t.Fatalf("file not written under workdir: %v", err)
	}
}

func TestReadDirectoryPage(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "a")
	write(t, dir, "b.txt", "b")
	out, err := run(t, registry, "read", `{"path":".","limit":1}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Read directory") || !strings.Contains(out, "a.txt") {
		t.Fatalf("unexpected directory output: %q", out)
	}
	if !strings.Contains(out, "Continue reading with offset: 2") {
		t.Fatalf("missing continuation hint: %q", out)
	}
}

func TestReadRejectsBinaryAndOutOfRange(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "bin.dat", "abc\x00def")
	if _, err := run(t, registry, "read", `{"path":"bin.dat"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("want binary error, got %v", err)
	}
	write(t, dir, "a.txt", "one\ntwo\n")
	if _, err := run(t, registry, "read", `{"path":"a.txt","offset":9}`, allowAll()); err == nil || !strings.Contains(err.Error(), "beyond") {
		t.Fatalf("want offset error, got %v", err)
	}
}

func TestFileToolsReportDiff(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	runDiff := func(name, input string) (string, string) {
		t.Helper()
		var diff string
		output, err := registry.Run(context.Background(), Call{
			Name:      name,
			Input:     []byte(input),
			Perm:      perm,
			SessionID: "test",
			OnDiff:    func(value string) { diff = value },
		})
		if err != nil {
			t.Fatal(err)
		}
		return output, diff
	}

	_, diff := runDiff("write", `{"path":"a.txt","content":"first\nsecond\n"}`)
	if !strings.Contains(diff, "--- a/a.txt") || !strings.Contains(diff, "+first") {
		t.Fatalf("write diff missing: %q", diff)
	}

	_, diff = runDiff("edit", `{"path":"a.txt","oldString":"first","newString":"changed"}`)
	if !strings.Contains(diff, "-first") || !strings.Contains(diff, "+changed") {
		t.Fatalf("edit diff missing: %q", diff)
	}
}

func TestLineDiffOmitsUnchangedEdges(t *testing.T) {
	diff := lineDiff("/work", "/work/sub/a.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	if !strings.Contains(diff, "--- a/sub/a.txt") || !strings.Contains(diff, "-two") || !strings.Contains(diff, "+TWO") {
		t.Fatalf("unexpected diff: %q", diff)
	}
	if strings.Contains(diff, "+one") || strings.Contains(diff, "-three") {
		t.Fatalf("unchanged lines leaked into diff: %q", diff)
	}
}

func TestDeniedToolDoesNotRun(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	perm := permission.New(permission.Rules{{Action: "edit", Resource: "*", Effect: permission.Deny}})

	_, err := run(t, registry, "write", `{"path":"a.txt","content":"x"}`, perm)
	if err == nil {
		t.Fatal("want permission error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want denied error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.txt")); statErr == nil {
		t.Fatal("file should not be written when denied")
	}
}

func TestOutsideWorkdirNeedsExternalPermission(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	rules := permission.Rules{
		{Action: "read", Resource: "*", Effect: permission.Allow},
		{Action: "external_directory", Resource: "*", Effect: permission.Deny},
	}
	perm := permission.New(rules)

	outside := filepath.Join(os.TempDir(), "lingzhu-external.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	if _, err := run(t, registry, "read", `{"path":"`+strings.ReplaceAll(outside, `\`, `\\`)+`"}`, perm); err == nil {
		t.Fatal("want external_directory denial")
	}
}

func TestUnknownTool(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	if _, err := run(t, registry, "nope", `{}`, nil); err == nil {
		t.Fatal("want error for unknown tool")
	}
}

func TestDefinitionsSorted(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	defs := registry.Definitions()
	if len(defs) != 8 {
		t.Fatalf("want 8 definitions, got %d", len(defs))
	}
	for i := 1; i < len(defs); i++ {
		if defs[i-1].Name > defs[i].Name {
			t.Fatalf("definitions not sorted: %v", defs)
		}
	}
}

func TestShellWorkdirAndTimeout(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sub/here.txt", "x")
	sh := &Shell{Command: platformShell(), Dir: dir}
	registry := NewRegistry()
	registry.Add(sh)

	out, err := run(t, registry, "shell", `{"command":`+quoteJSON(platformPwd())+`,"workdir":"sub"}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(out), "sub") {
		t.Fatalf("workdir ignored: %q", out)
	}
	if _, err := run(t, registry, "shell", `{"command":`+quoteJSON(platformSleep())+`,"timeout":50}`, allowAll()); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestShellStreamsOutput(t *testing.T) {
	registry := NewRegistry()
	registry.Add(&Shell{Command: platformShell(), Dir: t.TempDir()})
	var chunks []string
	_, err := registry.Run(context.Background(), Call{
		SessionID: "test",
		ID:        "call_stream",
		Name:      "shell",
		Input:     []byte(`{"command":` + quoteJSON(platformEcho()) + `}`),
		Perm:      allowAll(),
		Emit: func(ev event.Event) error {
			if ev.Type == event.ToolOutput {
				if chunk, ok := ev.Data["output"].(string); ok {
					chunks = append(chunks, chunk)
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(chunks, ""), "hi") {
		t.Fatalf("streamed output missing: %q", chunks)
	}
}

func platformShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}
	}
	return []string{"bash", "-lc"}
}

func platformPwd() string {
	if runtime.GOOS == "windows" {
		return "(Get-Location).Path"
	}
	return "pwd"
}

func platformSleep() string {
	if runtime.GOOS == "windows" {
		return "Start-Sleep -Seconds 3"
	}
	return "sleep 3"
}

func platformEcho() string {
	if runtime.GOOS == "windows" {
		return "Write-Output hi"
	}
	return "echo hi"
}

func TestShellBackgroundNotifies(t *testing.T) {
	dir := t.TempDir()
	jobs := job.New()
	defer jobs.Close()
	sh := &Shell{Command: platformShell(), Dir: dir, Jobs: jobs}
	registry := NewRegistry()
	registry.Add(sh)

	done := make(chan job.Job, 1)
	jobs.SetNotifier(func(item job.Job) { done <- item })

	out, err := run(t, registry, "shell", `{"command":`+quoteJSON(platformEcho())+`,"background":true}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "job ID:") || !strings.Contains(out, "notified") {
		t.Fatalf("unexpected background response: %q", out)
	}

	select {
	case item := <-done:
		if item.Status != job.Completed || item.ExitCode != 0 {
			t.Fatalf("unexpected job result: %+v", item)
		}
		if !strings.Contains(item.Tail, "hi") {
			t.Fatalf("job tail missing output: %q", item.Tail)
		}
		data, err := os.ReadFile(item.File)
		if err != nil || !strings.Contains(string(data), "hi") {
			t.Fatalf("job file missing output: %q %v", data, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job did not complete")
	}
}

func TestReadAttachesImageAndPDF(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	var imageBuffer bytes.Buffer
	if err := png.Encode(&imageBuffer, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	pngData := imageBuffer.Bytes()
	pdf := []byte("%PDF-1.4\nfake pdf\n")
	if err := os.WriteFile(filepath.Join(dir, "pixel.png"), pngData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}

	var attachments []model.Attachment
	call := func(name string) string {
		attachments = nil
		out, err := registry.Run(context.Background(), Call{
			Name:      "read",
			Input:     []byte(`{"path":"` + name + `"}`),
			Perm:      allowAll(),
			CanAttach: true,
			OnAttachment: func(attachment model.Attachment) {
				attachments = append(attachments, attachment)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := call("pixel.png")
	if !strings.Contains(out, "attached") || len(attachments) != 1 {
		t.Fatalf("image not attached: %q %+v", out, attachments)
	}
	if attachments[0].Mime != "image/png" || attachments[0].Name != "pixel.png" {
		t.Fatalf("unexpected image attachment: %+v", attachments[0])
	}
	if data, err := base64.StdEncoding.DecodeString(attachments[0].Data); err != nil || !bytes.Equal(data, pngData) {
		t.Fatalf("image bytes mismatch: %q %v", data, err)
	}

	out = call("doc.pdf")
	if !strings.Contains(out, "PDF") || len(attachments) != 1 || attachments[0].Mime != "application/pdf" {
		t.Fatalf("PDF not attached: %q %+v", out, attachments)
	}

	if _, err := registry.Run(context.Background(), Call{
		Name:      "read",
		Input:     []byte(`{"path":"pixel.png"}`),
		Perm:      allowAll(),
		CanAttach: false,
	}); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("want attachment capability error, got %v", err)
	}
}

func TestWriteAndEditRunFormatter(t *testing.T) {
	dir := t.TempDir()
	formatter := func(_ context.Context, path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		content := strings.TrimRight(string(data), "\n") + "\n// formatted\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
		return "fake", nil
	}

	write := &Write{Dir: dir}
	var diff string
	out, err := write.Run(context.Background(), Call{
		Name:     "write",
		Input:    []byte(`{"path":"a.go","content":"package main"}`),
		OnFormat: formatter,
		OnDiff:   func(value string) { diff = value },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(formatted with fake)") {
		t.Fatalf("unexpected write output: %q", out)
	}
	if !strings.Contains(diff, "// formatted") {
		t.Fatalf("diff should include formatted content: %q", diff)
	}

	edit := &Edit{Dir: dir}
	out, err = edit.Run(context.Background(), Call{
		Name:     "edit",
		Input:    []byte(`{"path":"a.go","oldString":"package main","newString":"package demo"}`),
		OnFormat: formatter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(formatted with fake)") {
		t.Fatalf("unexpected edit output: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "// formatted") != 2 {
		t.Fatalf("formatter did not run on both writes: %q", data)
	}
}

func TestReadResizesImageAttachment(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.png"), buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	enabled := true
	registry := NewRegistry()
	registry.Add(&Read{Dir: dir, Media: media.Config{Image: media.ImageConfig{
		AutoResize: &enabled, MaxWidth: 100, MaxHeight: 100, MaxBase64Bytes: 1 << 20,
	}}})
	var attachment model.Attachment
	if _, err := registry.Run(context.Background(), Call{
		Name: "read", Input: []byte(`{"path":"large.png"}`), Perm: allowAll(), CanAttach: true,
		OnAttachment: func(value model.Attachment) { attachment = value },
	}); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		// JPEG is also a valid smaller encoding.
		if _, jpegErr := jpeg.Decode(bytes.NewReader(data)); jpegErr != nil {
			t.Fatalf("resized image is not decodable: %v / %v", err, jpegErr)
		}
		return
	}
	if decoded.Bounds().Dx() > 100 || decoded.Bounds().Dy() > 100 {
		t.Fatalf("image not resized: %v", decoded.Bounds())
	}
}

func TestTruncateSavesFullOutput(t *testing.T) {
	t.Setenv("LINGZHU_HOME", t.TempDir())
	full := strings.Repeat("line\n", 3000)
	out := truncate(full)
	if !strings.Contains(out, "full output saved to ") {
		t.Fatalf("truncated output should name the saved file:\n%s", out)
	}
	start := strings.Index(out, "full output saved to ") + len("full output saved to ")
	path := strings.TrimSuffix(out[start:], "]")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != full {
		t.Fatalf("saved output differs: got %d bytes, want %d", len(data), len(full))
	}
}
