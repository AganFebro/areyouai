package promptbuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Builder struct {
	systemCoreText   string
	globalRulesText  string
	agentRulesText   string
	systemCoreHash   string
	globalRulesHash  string
	agentRulesHash   string
	maxRecentMessage int
	maxContextTokens int
}

type BuildInput struct {
	TaskContext    string
	RecentMessages []RecentMessage
}

type RecentMessage struct {
	Turn       int
	SenderID   string
	Ciphertext string
}

type Bundle struct {
	Prompt          string
	BundleHash      string
	SystemCoreHash  string
	GlobalRulesHash string
	AgentRulesHash  string
}

func NewDefaultBuilder() (*Builder, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}

	systemCore, err := os.ReadFile(filepath.Join(root, "prompt_layers", "SYSTEM_CORE.md"))
	if err != nil {
		return nil, err
	}
	globalRules, err := os.ReadFile(filepath.Join(root, "policies", "HARD_RULES_GLOBAL.md"))
	if err != nil {
		return nil, err
	}
	agentRules, err := os.ReadFile(filepath.Join(root, "prompt_layers", "HARD_RULES_AGENT.template.md"))
	if err != nil {
		return nil, err
	}

	systemCoreText := strings.TrimSpace(string(systemCore))
	globalRulesText := strings.TrimSpace(string(globalRules))
	agentRulesText := strings.TrimSpace(string(agentRules))

	return &Builder{
		systemCoreText:   systemCoreText,
		globalRulesText:  globalRulesText,
		agentRulesText:   agentRulesText,
		systemCoreHash:   hash(systemCoreText),
		globalRulesHash:  hash(globalRulesText),
		agentRulesHash:   hash(agentRulesText),
		maxRecentMessage: 6,
		maxContextTokens: 1000,
	}, nil
}

func (b *Builder) Build(in BuildInput) Bundle {
	recent := in.RecentMessages
	if len(recent) > b.maxRecentMessage {
		recent = recent[len(recent)-b.maxRecentMessage:]
	}
	recent = b.fitRecentToTokenCap(in.TaskContext, recent)

	var recentLines []string
	if len(recent) == 0 {
		recentLines = append(recentLines, "(empty)")
	} else {
		for _, m := range recent {
			recentLines = append(recentLines, fmt.Sprintf("- turn=%d sender=%s msg=%s", m.Turn, m.SenderID, m.Ciphertext))
		}
	}

	prompt := strings.Join([]string{
		"[SYSTEM_CORE]",
		b.systemCoreText,
		"",
		"[HARD_RULES_GLOBAL]",
		b.globalRulesText,
		"",
		"[HARD_RULES_AGENT]",
		b.agentRulesText,
		"",
		"[TASK_CONTEXT]",
		strings.TrimSpace(in.TaskContext),
		"",
		"[RECENT_MEMORY]",
		strings.Join(recentLines, "\n"),
	}, "\n")

	return Bundle{
		Prompt:          prompt,
		BundleHash:      hash(prompt),
		SystemCoreHash:  b.systemCoreHash,
		GlobalRulesHash: b.globalRulesHash,
		AgentRulesHash:  b.agentRulesHash,
	}
}

func (b *Builder) fitRecentToTokenCap(taskContext string, recent []RecentMessage) []RecentMessage {
	if len(recent) == 0 {
		return recent
	}

	fitted := make([]RecentMessage, len(recent))
	copy(fitted, recent)

	for len(fitted) > 1 {
		prompt := b.composePrompt(taskContext, fitted)
		if estimateTokens(prompt) <= b.maxContextTokens {
			return fitted
		}
		fitted = fitted[1:]
	}

	// If one message still exceeds cap, truncate ciphertext conservatively.
	last := fitted[0]
	maxChars := 280
	if len(last.Ciphertext) > maxChars {
		last.Ciphertext = last.Ciphertext[:maxChars] + "..."
	}
	fitted[0] = last
	return fitted
}

func (b *Builder) composePrompt(taskContext string, recent []RecentMessage) string {
	var recentLines []string
	if len(recent) == 0 {
		recentLines = append(recentLines, "(empty)")
	} else {
		for _, m := range recent {
			recentLines = append(recentLines, fmt.Sprintf("- turn=%d sender=%s msg=%s", m.Turn, m.SenderID, m.Ciphertext))
		}
	}
	return strings.Join([]string{
		"[SYSTEM_CORE]",
		b.systemCoreText,
		"",
		"[HARD_RULES_GLOBAL]",
		b.globalRulesText,
		"",
		"[HARD_RULES_AGENT]",
		b.agentRulesText,
		"",
		"[TASK_CONTEXT]",
		strings.TrimSpace(taskContext),
		"",
		"[RECENT_MEMORY]",
		strings.Join(recentLines, "\n"),
	}, "\n")
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Simple deterministic approximation: ~4 chars/token.
	return (len(text) + 3) / 4
}

func hash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve runtime caller")
	}
	// internal/service/promptbuilder/builder.go -> repo root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..")), nil
}
