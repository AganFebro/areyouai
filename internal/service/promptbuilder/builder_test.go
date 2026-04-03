package promptbuilder

import (
	"strings"
	"testing"
)

func TestBuildIncludesCanonicalIdentityStack(t *testing.T) {
	t.Parallel()

	b, err := NewDefaultBuilder()
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	out := b.Build(BuildInput{
		TaskContext: "room_id=room_1",
		RecentMessages: []RecentMessage{
			{Turn: 0, SenderID: "agt_a", Ciphertext: "hello"},
		},
	})

	wantOrder := []string{
		"[SYSTEM_CORE]",
		"[HARD_RULES_GLOBAL]",
		"[HARD_RULES_AGENT]",
		"[IDENTITY]",
		"[SOUL]",
		"[USER]",
		"[TASK_CONTEXT]",
		"[RECENT_MEMORY]",
	}
	prev := -1
	for _, marker := range wantOrder {
		idx := strings.Index(out.Prompt, marker)
		if idx == -1 {
			t.Fatalf("missing marker %s", marker)
		}
		if idx <= prev {
			t.Fatalf("marker %s out of order", marker)
		}
		prev = idx
	}

	if out.BundleHash == "" || out.SystemCoreHash == "" || out.GlobalRulesHash == "" || out.AgentRulesHash == "" {
		t.Fatalf("missing baseline hashes: %+v", out)
	}
	if out.IdentityHash == "" || out.SoulHash == "" || out.UserHash == "" {
		t.Fatalf("missing identity hashes: %+v", out)
	}
	wantStack := []string{"SYSTEM_CORE", "HARD_RULES_GLOBAL", "HARD_RULES_AGENT", "IDENTITY", "SOUL", "USER", "TASK_CONTEXT", "RECENT_MEMORY"}
	if len(out.OrderedStack) != len(wantStack) {
		t.Fatalf("ordered stack len=%d want=%d", len(out.OrderedStack), len(wantStack))
	}
	for i, want := range wantStack {
		if out.OrderedStack[i] != want {
			t.Fatalf("ordered stack[%d]=%q want=%q", i, out.OrderedStack[i], want)
		}
	}
	if got := b.composePrompt("room_id=room_1", []RecentMessage{{Turn: 0, SenderID: "agt_a", Ciphertext: "hello"}}); got != out.Prompt {
		t.Fatal("composePrompt and Build diverged")
	}
}
