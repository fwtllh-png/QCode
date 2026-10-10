package guard

import (
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestApprovalRecoveryCannotExtendCurrentExpiry(t *testing.T) {
	now := time.Now()
	current := ApprovalRequest{CallID: "call", Tool: "exec_command", ArgumentsDigest: "arguments", BindingDigest: "binding",
		AllowedScopes: []policy.ApprovalScope{policy.ApprovalOnce}, ExpiresAt: now.Add(time.Minute)}
	for _, test := range []struct {
		name   string
		expiry time.Time
		valid  bool
	}{
		{"earlier", now.Add(time.Second), true},
		{"same", current.ExpiresAt, true},
		{"later", now.Add(time.Hour), false},
		{"unbounded", time.Time{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			saved := current
			saved.ExpiresAt = test.expiry
			if err := validateRecoveredApproval(saved, current); (err == nil) != test.valid {
				t.Fatalf("recovery=%v", err)
			}
		})
	}
}
