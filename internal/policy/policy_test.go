package policy

import "testing"

func TestEvaluateLastMatchWins(t *testing.T) {
	rules := Normalize([]Statement{
		{Action: "provider.use", Resource: "*", Effect: Deny},
		{Action: "provider.use", Resource: "anthropic", Effect: Allow},
		{Action: "tool.use", Resource: "shell:git push *", Effect: Deny},
		{Action: "tool.use", Resource: "shell:git status *", Effect: Allow},
	})
	if !rules.Denied("provider.use", "openai") {
		t.Fatal("broad deny should block openai")
	}
	if rules.Denied("provider.use", "anthropic") {
		t.Fatal("later allow should lift the broad deny")
	}
	if !rules.Denied("tool.use", "shell:git push origin") {
		t.Fatal("shell push should be denied")
	}
	if rules.Denied("tool.use", "shell:git status") {
		t.Fatal("shell status should be allowed")
	}
	if rules.Denied("tool.use", "edit:src/a.go") {
		t.Fatal("unmatched actions default to allow")
	}
}

func TestNormalizeDropsInvalidStatements(t *testing.T) {
	rules := Normalize([]Statement{
		{Action: "", Resource: "*", Effect: Deny},
		{Action: "mcp.use", Resource: "", Effect: Deny},
		{Action: "skill.use", Resource: "*", Effect: "ask"},
		{Action: "plugin.use", Resource: "demo", Effect: "deny"},
	})
	if len(rules) != 1 || rules[0].Action != "plugin.use" || rules[0].Effect != Deny {
		t.Fatalf("unexpected normalized rules: %+v", rules)
	}
}

func TestDeniedAll(t *testing.T) {
	rules := Normalize([]Statement{{Action: "tool.use", Resource: "edit:*.env", Effect: Deny}})
	if !rules.DeniedAll("tool.use", []string{"edit:a.env", "edit:b.txt"}) {
		t.Fatal("one denied resource should deny the operation")
	}
	if rules.DeniedAll("tool.use", []string{"edit:b.txt", "edit:c.txt"}) {
		t.Fatal("allowed resources should not deny")
	}
}
