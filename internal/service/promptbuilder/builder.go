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
	identityText     string
	soulText         string
	userText         string
	orderedStack     []string
	systemCoreHash   string
	globalRulesHash  string
	agentRulesHash   string
	identityHash     string
	soulHash         string
	userHash         string
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
	OrderedStack    []string
	SystemCoreHash  string
	GlobalRulesHash string
	AgentRulesHash  string
	IdentityHash    string
	SoulHash        string
	UserHash        string
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
	identity, err := os.ReadFile(filepath.Join(root, "prompt_layers", "IDENTITY.default.md"))
	if err != nil {
		return nil, err
	}
	soul, err := os.ReadFile(filepath.Join(root, "prompt_layers", "SOUL.default.md"))
	if err != nil {
		return nil, err
	}
	user, err := os.ReadFile(filepath.Join(root, "prompt_layers", "USER.default.md"))
	if err != nil {
		return nil, err
	}

	systemCoreText := strings.TrimSpace(string(systemCore))
	globalRulesText := strings.TrimSpace(string(globalRules))
	agentRulesText := strings.TrimSpace(string(agentRules))
	identityText := strings.TrimSpace(string(identity))
	soulText := strings.TrimSpace(string(soul))
	userText := strings.TrimSpace(string(user))

	return &Builder{
		systemCoreText:   systemCoreText,
		globalRulesText:  globalRulesText,
		agentRulesText:   agentRulesText,
		identityText:     identityText,
		soulText:         soulText,
		userText:         userText,
		orderedStack:     []string{"SYSTEM_CORE", "HARD_RULES_GLOBAL", "HARD_RULES_AGENT", "IDENTITY", "SOUL", "USER", "TASK_CONTEXT", "RECENT_MEMORY"},
		systemCoreHash:   hash(systemCoreText),
		globalRulesHash:  hash(globalRulesText),
		agentRulesHash:   hash(agentRulesText),
		identityHash:     hash(identityText),
		soulHash:         hash(soulText),
		userHash:         hash(userText),
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
	prompt := b.renderCanonicalPrompt(in.TaskContext, recent)

	return Bundle{
		Prompt:          prompt,
		BundleHash:      hash(prompt),
		OrderedStack:    append([]string(nil), b.orderedStack...),
		SystemCoreHash:  b.systemCoreHash,
		GlobalRulesHash: b.globalRulesHash,
		AgentRulesHash:  b.agentRulesHash,
		IdentityHash:    b.identityHash,
		SoulHash:        b.soulHash,
		UserHash:        b.userHash,
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
	return b.renderCanonicalPrompt(taskContext, recent)
}

type promptSection struct {
	marker string
	text   string
}

func (b *Builder) renderCanonicalPrompt(taskContext string, recent []RecentMessage) string {
	sections := []promptSection{
		{marker: "SYSTEM_CORE", text: b.systemCoreText},
		{marker: "HARD_RULES_GLOBAL", text: b.globalRulesText},
		{marker: "HARD_RULES_AGENT", text: b.agentRulesText},
		{marker: "IDENTITY", text: b.identityText},
		{marker: "SOUL", text: b.soulText},
		{marker: "USER", text: b.userText},
		{marker: "TASK_CONTEXT", text: strings.TrimSpace(taskContext)},
		{marker: "RECENT_MEMORY", text: renderRecentMemory(recent)},
	}
	parts := make([]string, 0, len(sections)*3)
	for _, section := range sections {
		parts = append(parts, fmt.Sprintf("[%s]", section.marker), section.text, "")
	}
	return strings.TrimRight(strings.Join(parts, "\n"), "\n")
}

func renderRecentMemory(recent []RecentMessage) string {
	if len(recent) == 0 {
		return "(empty)"
	}
	lines := make([]string, 0, len(recent))
	for _, m := range recent {
		lines = append(lines, fmt.Sprintf("- turn=%d sender=%s msg=%s", m.Turn, m.SenderID, m.Ciphertext))
	}
	return strings.Join(lines, "\n")
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
