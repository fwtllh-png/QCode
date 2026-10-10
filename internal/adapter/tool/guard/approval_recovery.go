package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// Recovery may restore a wait, never change the operation the user saw or
// widen today's constraints. A changed request must obtain a new approval.
func validateRecoveredApproval(saved, current ApprovalRequest) error {
	if saved.BindingDigest == "" || saved.BindingDigest != current.BindingDigest ||
		saved.CallID != current.CallID || saved.Tool != current.Tool || saved.ArgumentsDigest != current.ArgumentsDigest {
		return staleRecoveredApproval()
	}
	if !current.ExpiresAt.IsZero() && (saved.ExpiresAt.IsZero() || saved.ExpiresAt.After(current.ExpiresAt)) {
		return staleRecoveredApproval()
	}
	for _, scope := range saved.AllowedScopes {
		if !slices.Contains(current.AllowedScopes, scope) {
			return staleRecoveredApproval()
		}
	}
	if len(saved.AllowedScopes) == 0 || (saved.ReplacementAllowed && !current.ReplacementAllowed) {
		return staleRecoveredApproval()
	}
	return nil
}

// Bind the full Guard request before the public projection drops internal
// grant/network details. Recovery compares against a freshly prepared request,
// never reconstructs authority from this digest or the historical review.
func approvalBindingDigest(request ApprovalRequest, identity tool.InvocationIdentity) string {
	request.RequestID, request.BindingDigest = "", ""
	request.GuardianReason, request.GuardianReasonCode, request.GuardianReviewID = "", "", ""
	// ArgumentsDigest already covers canonical arguments; neither presentation
	// formatting nor an expiry timestamp changes the operation's identity.
	request.Arguments = nil
	request.ExpiresAt = time.Time{}
	body, err := json.Marshal(struct {
		Request  ApprovalRequest
		Identity tool.InvocationIdentity
	}{request, identity})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func staleRecoveredApproval() error {
	return &policy.DecisionError{Code: "approval_recovery_stale", Reason: "restored approval no longer matches the current operation or permission constraints"}
}

// DiscardRecoveredApproval retires only a wait that has not been reconnected.
// Runtime calls it when the bound tool fails before reaching approval.
func (g *Guard) DiscardRecoveredApproval(callID string) {
	g.mu.Lock()
	delete(g.recovered, callID)
	g.mu.Unlock()
}
