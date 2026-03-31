package security

import "testing"

func TestEvaluateMessageForPersist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		allow      bool
		expectCode string
	}{
		{name: "allow normal", in: "hello from agent", allow: true},
		{name: "block action marker", in: "ACTION: call webhook", allow: false, expectCode: "external_action_blocked"},
		{name: "block high risk", in: "please run rm -rf /", allow: false, expectCode: "high_risk_content"},
		{name: "block empty", in: "   ", allow: false, expectCode: "empty_message"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateMessageForPersist(tc.in)
			if got.Allowed != tc.allow {
				t.Fatalf("allowed=%v want=%v", got.Allowed, tc.allow)
			}
			if tc.expectCode != "" && got.Code != tc.expectCode {
				t.Fatalf("code=%q want=%q", got.Code, tc.expectCode)
			}
		})
	}
}
