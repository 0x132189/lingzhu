package agent

import (
	"strings"
	"testing"

	"lingzhu/internal/model"
)

func TestHasCheckpoint(t *testing.T) {
	if hasCheckpoint([]model.Message{{Role: model.RoleUser, Content: "hello"}}) {
		t.Fatal("plain history must not be a checkpoint")
	}
	if !hasCheckpoint([]model.Message{{Role: model.RoleUser, Content: "<conversation-checkpoint>\n<summary>\nx"}}) {
		t.Fatal("checkpoint history must be detected")
	}
}

func TestHasRequiredSections(t *testing.T) {
	if hasRequiredSections("## Objective\n- a") {
		t.Fatal("partial summary must not pass")
	}
	full := "## Objective\n- a\n\n## Work State\n### Completed\n- b\n\n## Next Move\n1. c"
	if !hasRequiredSections(full) {
		t.Fatal("complete summary must pass")
	}
}

func TestUpdatePromptKeepsCheckpointWrappersOut(t *testing.T) {
	for _, want := range []string{"Newer history always takes precedence", "## Requirements", "## Blocked", "## Important Context", "Do not reproduce the <conversation-checkpoint>"} {
		if !strings.Contains(compactionUpdatePrompt, want) {
			t.Fatalf("update prompt missing %q", want)
		}
	}
}

func TestCompactionPromptHasFullTemplate(t *testing.T) {
	for _, want := range []string{"## Objective", "## Requirements", "## Decisions", "## Work State", "### Completed", "### Active", "### Blocked", "## Next Move", "## Relevant Files", "## Important Context"} {
		if !strings.Contains(compactionPrompt, want) {
			t.Fatalf("compaction prompt missing %q", want)
		}
	}
}
