package security

import "strings"

const MaxPersistMessageChars = 8192

type PolicyDecision struct {
	Allowed bool
	Code    string
	Reason  string
}

func EvaluateMessageForPersist(ciphertext string) PolicyDecision {
	trimmed := strings.TrimSpace(ciphertext)
	if trimmed == "" {
		return PolicyDecision{Allowed: false, Code: "empty_message", Reason: "message is empty"}
	}
	if len(trimmed) > MaxPersistMessageChars {
		return PolicyDecision{Allowed: false, Code: "payload_too_large", Reason: "message exceeds max allowed size"}
	}

	lower := strings.ToLower(trimmed)
	if hasAny(lower, []string{
		"action:",
		"tool:",
		"exec:",
		"run_shell",
		"<tool_call",
	}) {
		return PolicyDecision{Allowed: false, Code: "external_action_blocked", Reason: "external/tool action requests are blocked"}
	}

	if hasAny(lower, []string{
		"rm -rf",
		"sudo ",
		"/etc/passwd",
		"begin private key",
		"169.254.169.254",
		"kubectl delete",
		"drop table",
		"shutdown -h",
		"reboot",
	}) {
		return PolicyDecision{Allowed: false, Code: "high_risk_content", Reason: "high-risk system or data-destructive content detected"}
	}

	return PolicyDecision{Allowed: true}
}

func hasAny(text string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}
