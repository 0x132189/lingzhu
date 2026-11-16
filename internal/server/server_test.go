package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/imagegen"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/question"
	"lingzhu/internal/schedule"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type staticProvider struct{}

func (staticProvider) Stream(context.Context, model.Request) (provider.Stream, error) {
	return &staticStream{chunks: []provider.Chunk{{Text: "hi"}, {Text: "!"}}}, nil
}

type staticStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *staticStream) Recv() (provider.Chunk, error) {
	if s.i >= len(s.chunks) {
		return provider.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.i]
	s.i++
	return chunk, nil
}

func (s *staticStream) Close() error { return nil }

func testStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newServerWith(t, staticProvider{})
}

func newServerWith(t *testing.T, p provider.Provider) *Server {
	t.Helper()
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "test-model", Name: "test-model"}}, "test-model")
	return New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store)
}

// blockProvider never answers until its run is cancelled.
type blockProvider struct {
	started chan struct{}
}

func (p *blockProvider) Stream(ctx context.Context, _ model.Request) (provider.Stream, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	return &blockStream{ctx: ctx}, nil
}

type blockStream struct {
	ctx context.Context
}

func (s *blockStream) Recv() (provider.Chunk, error) {
	<-s.ctx.Done()
	return provider.Chunk{}, s.ctx.Err()
}

func (s *blockStream) Close() error { return nil }

func prompt(t *testing.T, url, session, text string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/sessions/"+session+"/prompt", "application/json", strings.NewReader(`{"text":"`+text+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPromptStreamsSSE(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json",
		strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("want SSE content type, got %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"event: text.delta", "event: run.end", `"reason":"done"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}

	messages, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer messages.Body.Close()
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(messages.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 2 || payload.Messages[1].Content != "hi!" {
		t.Fatalf("unexpected messages: %+v", payload.Messages)
	}
}

// gateProvider pauses the first response until the test releases it, so input
// can be queued while the turn is active.
type gateProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (p *gateProvider) Stream(context.Context, model.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		select {
		case p.started <- struct{}{}:
		default:
		}
		return &gateStream{release: p.release}, nil
	}
	return &staticStream{chunks: []provider.Chunk{{Text: "second"}}}, nil
}

type gateStream struct {
	release <-chan struct{}
	sent    bool
}

func (s *gateStream) Recv() (provider.Chunk, error) {
	if s.sent {
		return provider.Chunk{}, io.EOF
	}
	<-s.release
	s.sent = true
	return provider.Chunk{Text: "first"}, nil
}

func (s *gateStream) Close() error { return nil }

func TestInputQueuesWhileRunIsActive(t *testing.T) {
	p := &gateProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 4}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}
	running, err := http.Get(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	var runningInfo struct {
		Running bool `json:"running"`
	}
	if err := json.NewDecoder(running.Body).Decode(&runningInfo); err != nil {
		t.Fatal(err)
	}
	running.Body.Close()
	if !runningInfo.Running {
		t.Fatal("session should report running while the model is active")
	}

	queued := post(t, srv.URL+"/sessions/s1/input", `{"text":"queued"}`)
	if queued.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for queued input, got %d", queued.StatusCode)
	}
	close(p.release)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	running, err = http.Get(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(running.Body).Decode(&runningInfo); err != nil {
		t.Fatal(err)
	}
	running.Body.Close()
	if runningInfo.Running {
		t.Fatal("session should be idle after run.end")
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "first", "queued", "second"}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %+v", len(want), messages)
	}
	for index, text := range want {
		if messages[index].Content != text {
			t.Fatalf("message %d = %q, want %q", index, messages[index].Content, text)
		}
	}
}

func TestQueueDeliveryStartsNewTurn(t *testing.T) {
	p := &gateProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 4}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}

	queued, err := http.Post(srv.URL+"/sessions/s1/input", "application/json", strings.NewReader(`{"text":"later","delivery":"queue"}`))
	if err != nil {
		t.Fatal(err)
	}
	if queued.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for queued delivery, got %d", queued.StatusCode)
	}
	var accepted struct {
		Queued   bool   `json:"queued"`
		Delivery string `json:"delivery"`
	}
	if err := json.NewDecoder(queued.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	queued.Body.Close()
	if !accepted.Queued || accepted.Delivery != "queue" {
		t.Fatalf("unexpected accepted payload: %+v", accepted)
	}

	inbox, err := http.Get(srv.URL + "/sessions/s1/inbox")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data []struct {
			Delivery string `json:"delivery"`
			Payload  struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.NewDecoder(inbox.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	inbox.Body.Close()
	if len(listed.Data) != 1 || listed.Data[0].Delivery != "queue" || listed.Data[0].Payload.Text != "later" {
		t.Fatalf("unexpected inbox while running: %+v", listed.Data)
	}

	close(p.release)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := []string{"start", "first", "later", "second"}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %+v", len(want), messages)
	}
	for index, text := range want {
		if messages[index].Content != text {
			t.Fatalf("message %d = %q, want %q", index, messages[index].Content, text)
		}
	}
}

func TestInputWakesIdleSession(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp := post(t, srv.URL+"/sessions/s1/input", `{"text":"late"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 without a run, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGlobalEventsTagSessions(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	lines := make(chan string, 128)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	go func() {
		response, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"hello"}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()

	timeout := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("global event stream closed")
			}
			if strings.HasPrefix(line, "data: ") &&
				strings.Contains(line, `"sessionId":"s1"`) &&
				strings.Contains(line, `"type":"run.end"`) {
				return
			}
		case <-timeout:
			t.Fatal("global event stream did not report run.end")
		}
	}
}

func TestListSessionsEmptyIsAnArray(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Sessions []session.Info `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Sessions == nil {
		t.Fatalf("empty session list must encode as [], got %s", rec.Body.String())
	}
}

func TestPromptRejectsEmptyText(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestReplyPermissionValidatesInput(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	bad := post(t, srv.URL+"/sessions/s1/permission/per_1", `{"decision":"maybe"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown decision, got %d", bad.StatusCode)
	}
	missing := post(t, srv.URL+"/sessions/s1/permission/per_missing", `{"decision":"once"}`)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for unknown request, got %d", missing.StatusCode)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

type recordingProvider struct {
	model string
}

func (p *recordingProvider) Stream(_ context.Context, req model.Request) (provider.Stream, error) {
	p.model = req.Model
	return &staticStream{chunks: []provider.Chunk{{Text: "ok"}}}, nil
}

func TestSessionModelSelectionRoundTrip(t *testing.T) {
	store := testStore(t)
	recorder := &recordingProvider{}
	catalog := provider.Static("p1", "P1", recorder, []provider.ModelSpec{
		{ID: "m1", ContextWindow: 1000},
		{ID: "m2", ContextWindow: 2000},
	}, "m1")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json",
		strings.NewReader(`{"text":"hello","provider":"p1","model":"m2"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if recorder.model != "m2" {
		t.Fatalf("prompt used %q, want m2", recorder.model)
	}
	if info, err := store.Info("s1"); err != nil || info.Provider != "p1" || info.Model != "m2" {
		t.Fatalf("selection was not stored: %+v %v", info, err)
	}

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/sessions/s1", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	patch, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	patch.Body.Close()
	if patch.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", patch.StatusCode)
	}

	contextResp, err := http.Get(srv.URL + "/sessions/s1/context")
	if err != nil {
		t.Fatal(err)
	}
	defer contextResp.Body.Close()
	var contextPayload struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Window   int    `json:"window"`
	}
	if err := json.NewDecoder(contextResp.Body).Decode(&contextPayload); err != nil {
		t.Fatal(err)
	}
	if contextPayload.Provider != "p1" || contextPayload.Model != "m1" || contextPayload.Window != 1000 {
		t.Fatalf("unexpected context: %+v", contextPayload)
	}
}

type questionProvider struct {
	mu   sync.Mutex
	step int
}

func (p *questionProvider) Stream(context.Context, model.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.step++
	step := p.step
	p.mu.Unlock()
	if step == 1 {
		return &staticStream{chunks: []provider.Chunk{{
			ToolCalls: []provider.ToolCallDelta{{
				Index:     0,
				ID:        "call_q",
				Name:      "question",
				Arguments: `{"questions":[{"question":"Pick a color","header":"Color","options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]}]}`,
			}},
		}}}, nil
	}
	return &staticStream{chunks: []provider.Chunk{{Text: "answered"}}}, nil
}

func TestQuestionEndpointAnswersTool(t *testing.T) {
	store := testStore(t)
	manager := question.New()
	catalog := provider.Static("test", "Test", &questionProvider{}, []provider.ModelSpec{{ID: "test-model", Name: "test-model"}}, "test-model")
	registry := tool.NewRegistry()
	registry.Add(question.NewTool(manager))
	a := agent.New(store, catalog, registry, nil, agent.Options{MaxSteps: 4, Questions: manager})
	srv := httptest.NewServer(New(a, store).Handler())
	defer srv.Close()

	type finished struct {
		resp *http.Response
		err  error
	}
	done := make(chan finished, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"ask me"}`))
		done <- finished{resp: resp, err: err}
	}()

	var request question.Request
	for i := 0; i < 100 && request.ID == ""; i++ {
		resp, err := http.Get(srv.URL + "/sessions/s1/question")
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Questions []question.Request `json:"questions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(payload.Questions) > 0 {
			request = payload.Questions[0]
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if request.ID == "" {
		t.Fatal("question never became pending")
	}
	if request.SessionID != "s1" || len(request.Questions) != 1 || request.Questions[0].Header != "Color" {
		t.Fatalf("unexpected question: %+v", request)
	}

	reply := func(session, qid, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/sessions/"+session+"/question/"+qid, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := reply("other", request.ID, `{"answers":[["Red"]]}`); code != http.StatusNotFound {
		t.Fatalf("wrong session status = %d", code)
	}
	if code := reply("s1", request.ID, `{"answers":[]}`); code != http.StatusBadRequest {
		t.Fatalf("bad answer count status = %d", code)
	}
	if code := reply("s1", request.ID, `{"answers":[["Red"]]}`); code != http.StatusNoContent {
		t.Fatalf("reply status = %d", code)
	}

	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	_, _ = io.Copy(io.Discard, result.resp.Body)
	result.resp.Body.Close()

	var messages struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	resp, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages.Messages {
		if strings.Contains(message.Content, `"Pick a color"="Red"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("answer did not reach the model: %+v", messages.Messages)
	}
}

func newServerWithStore(t *testing.T, p provider.Provider, store *session.Store) *Server {
	t.Helper()
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "test-model", Name: "test-model"}}, "test-model")
	return New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store)
}

func TestCommandsEndpointEmpty(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/commands", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var body struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Commands == nil || len(body.Commands) != 0 {
		t.Fatalf("commands should be an empty array: %s", rec.Body.String())
	}
}

func TestAgentsEndpoint(t *testing.T) {
	server := newTestServer(t)
	server.SetAgents(agents.New(agents.Config{Agents: map[string]agents.Definition{
		"reviewer": {Name: "Reviewer", Description: "Reviews code", Mode: agents.ModePrimary},
	}}, ""))

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var body struct {
		Agents  []agents.Profile `json:"agents"`
		Default string           `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, profile := range body.Agents {
		ids[profile.ID] = true
	}
	if body.Default != "build" || !ids["build"] || !ids["plan"] || !ids["reviewer"] {
		t.Fatalf("unexpected agents: %s", rec.Body.String())
	}
}

func TestAgentEndpointServesHiddenProfiles(t *testing.T) {
	server := newTestServer(t)
	server.SetAgents(agents.New(agents.Config{}, ""))

	// A selectable agent resolves normally.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/plan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	var profile agents.Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.ID != "plan" || profile.Name != "Plan" {
		t.Fatalf("unexpected profile: %+v", profile)
	}

	// A hidden profile still resolves, so a client can label old messages.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/compaction", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hidden profiles should resolve: %d %s", rec.Code, rec.Body.String())
	}
	// An unknown id is a 404, not an empty success.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agents should be 404, got %d", rec.Code)
	}
}

func TestPromptRejectsUnknownAgent(t *testing.T) {
	server := newTestServer(t)
	body := strings.NewReader(`{"text":"hi","agent":"missing"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/prompt", body))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown agent") {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRenameSelectsAgent(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.store.Append("s1", event.User("hi")); err != nil {
		t.Fatal(err)
	}
	server.SetAgents(agents.New(agents.Config{}, ""))
	body := strings.NewReader(`{"agent":"plan"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	meta, err := server.store.Meta("s1")
	if err != nil || meta.Agent != "plan" {
		t.Fatalf("agent not stored: %+v %v", meta, err)
	}
}

func TestImageStatusEndpoint(t *testing.T) {
	server := newTestServer(t)
	server.SetImage(imagegen.New(imagegen.Config{APIKey: "secret", Model: "gpt-image-1", Size: "1024x1024"}))

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var body struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
		Model      string `json:"model"`
		Size       string `json:"size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Provider != "openai" || !body.Configured || body.Model != "gpt-image-1" || body.Size != "1024x1024" {
		t.Fatalf("unexpected image status: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("image status leaked the API key")
	}
}

func TestScheduleListEndpoint(t *testing.T) {
	server := newTestServer(t)
	manager, err := schedule.New(t.TempDir(), schedule.Config{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	server.SetSchedules(manager)
	if _, err := manager.Create("s1", schedule.CreateRequest{Prompt: "one", When: schedule.When{Type: "in", In: "5m"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create("s2", schedule.CreateRequest{Prompt: "two", When: schedule.When{Type: "every", Every: "1h"}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var all struct {
		Schedules []schedule.Item `json:"schedules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Schedules) != 2 {
		t.Fatalf("unexpected all schedules: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules?sessionId=s1", nil))
	var filtered struct {
		Schedules []schedule.Item `json:"schedules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Schedules) != 1 || filtered.Schedules[0].Prompt != "one" {
		t.Fatalf("unexpected filtered schedules: %s", rec.Body.String())
	}
}
