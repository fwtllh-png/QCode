package policy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestApprovalCacheHostScopedSessionReuse(t *testing.T) {
	cache := NewApprovalCache()
	now := time.Now()
	first := Invocation{
		CallID: "c1", Tool: "web_fetch",
		Arguments: json.RawMessage(`{"url":"https://example.com/a"}`),
		Resources: []tool.Resource{
			{Kind: "url", ID: "https://example.com/a", Access: tool.AccessRead},
			{Kind: "host", ID: "example.com", Access: tool.AccessRead},
		},
		Capability: CapabilityNetwork, Validated: true,
	}
	request, err := NewApprovalRequestForScope(first, ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	second := Invocation{
		CallID: "c2", Tool: "web_fetch",
		Arguments: json.RawMessage(`{"url":"https://example.com/b"}`),
		Resources: []tool.Resource{
			{Kind: "url", ID: "https://example.com/b", Access: tool.AccessRead},
			{Kind: "host", ID: "example.com", Access: tool.AccessRead},
		},
		Capability: CapabilityNetwork, Validated: true,
	}
	if !cache.MatchInvocation(second, now) {
		t.Fatal("same host should reuse session approval")
	}
	other := Invocation{
		CallID: "c3", Tool: "web_fetch",
		Arguments: json.RawMessage(`{"url":"https://other.com/"}`),
		Resources: []tool.Resource{
			{Kind: "url", ID: "https://other.com/", Access: tool.AccessRead},
			{Kind: "host", ID: "other.com", Access: tool.AccessRead},
		},
		Capability: CapabilityNetwork, Validated: true,
	}
	if cache.MatchInvocation(other, now) {
		t.Fatal("different host must not reuse")
	}
}
