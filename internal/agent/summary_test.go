package agent

import (
	"context"
	"strings"
	"testing"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

func TestSummarizeUsesHiddenSummaryAgent(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.User("do the work")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "I changed the parser."}}}}
	profiles := agents.New(agents.Config{}, "")
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{Agents: profiles})

	summary, err := a.Summarize(context.Background(), "s1", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "I changed the parser." {
		t.Fatalf("unexpected summary: %q", summary)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("want one summary request, got %d", len(fake.requests))
	}
	request := fake.requests[0]
	if len(request.Tools) != 0 {
		t.Fatalf("summary request must not offer tools: %+v", request.Tools)
	}
	if len(request.Messages) < 2 || request.Messages[0].Role != model.RoleSystem || !strings.Contains(request.Messages[0].Content, "pull request description") {
		t.Fatalf("summary prompt missing: %+v", request.Messages)
	}
	if last := request.Messages[len(request.Messages)-1]; last.Role != model.RoleUser || last.Content != SummarizePrompt {
		t.Fatalf("summary user turn missing: %+v", last)
	}
	profile, ok := profiles.Get("summary")
	if !ok || profile.Permissions.Evaluate("shell", "*") != permission.Deny {
		t.Fatalf("summary agent must keep deny-all permissions: %+v", profile)
	}
}
