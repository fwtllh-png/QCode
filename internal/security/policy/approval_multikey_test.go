package policy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestApprovalCacheTypedFileGrantRequiresExactPathSet(t *testing.T) {
	now := time.Unix(5000, 0)
	cache := NewApprovalCache()
	a := tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}
	b := tool.Resource{Kind: "file", Path: "b.go", Access: tool.AccessWrite}
	c := tool.Resource{Kind: "file", Path: "c.go", Access: tool.AccessWrite}

	first := Invocation{
		CallID: "1", Tool: "file_patch", Arguments: []byte(`{"diff":"ab"}`),
		Resources: []tool.Resource{a, b}, Capability: CapabilityWrite,
		Access: tool.AccessTree, Sandbox: tool.SandboxStrong, Journaled: true, Validated: true,
	}
	request, err := NewApprovalRequestForScope(first, ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}

	subset := Invocation{
		CallID: "2", Tool: "file_patch", Arguments: []byte(`{"diff":"a-only"}`),
		Resources: []tool.Resource{a}, Capability: CapabilityWrite,
		Access: tool.AccessTree, Sandbox: tool.SandboxStrong, Journaled: true, Validated: true,
	}
	if cache.MatchInvocation(subset, now) {
		t.Fatal("path subset must not inherit a broader transaction grant")
	}

	bothAgain := Invocation{
		CallID: "3", Tool: "file_patch", Arguments: []byte(`{"diff":"ab2"}`),
		Resources: []tool.Resource{a, b}, Capability: CapabilityWrite,
		Access: tool.AccessTree, Sandbox: tool.SandboxStrong, Journaled: true, Validated: true,
	}
	if !cache.MatchInvocation(bothAgain, now) {
		t.Fatal("full previously-approved set should skip ask")
	}

	partial := Invocation{
		CallID: "4", Tool: "file_patch", Arguments: []byte(`{"diff":"ac"}`),
		Resources: []tool.Resource{a, c}, Capability: CapabilityWrite,
		Access: tool.AccessTree, Sandbox: tool.SandboxStrong, Journaled: true, Validated: true,
	}
	if cache.MatchInvocation(partial, now) {
		t.Fatal("unapproved path must still ask")
	}
}

func shellGrantInvocation(
	callID, command, cwd string, resources ...tool.Resource,
) Invocation {
	return Invocation{
		CallID: callID, Tool: "exec_command", Capability: CapabilityProcess,
		Arguments: []byte(
			`{"command":` + quoteJSON(command) + `,"cwd":` + quoteJSON(cwd) + `}`,
		),
		Resources: resources, Access: tool.AccessWrite,
		Sandbox: tool.SandboxStrong, Validated: true,
	}
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestApprovalCacheShellGrantPrefixCoversArgvExtensions(t *testing.T) {
	now := time.Unix(5000, 0)
	cache := NewApprovalCache()
	resources := []tool.Resource{
		{Kind: "file", Path: "bin/app", Access: tool.AccessWrite},
	}
	approved := shellGrantInvocation("1", "go build -o bin/app ./...", ".", resources...)
	request, err := NewApprovalRequestForScope(approved, ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	if request.Grant.Prefix == nil {
		t.Fatalf("static single-segment command has no prefix grant: %+v", request.Grant)
	}
	// Same argv plus extra trailing flags reuses the approval.
	extended := shellGrantInvocation("2", "go build -o bin/app ./... -v", ".", resources...)
	if !cache.MatchInvocation(extended, now) {
		t.Fatal("argv extension of an approved command should skip ask")
	}
	// Different arguments are not extensions and must ask.
	different := shellGrantInvocation("3", "go build -o bin/other ./...", ".", resources...)
	if cache.MatchInvocation(different, now) {
		t.Fatal("different target must still ask")
	}
	// Composite candidates never prefix-match an approved segment.
	composite := shellGrantInvocation(
		"4", "go build -o bin/app ./... && printf done", ".", resources...,
	)
	if cache.MatchInvocation(composite, now) {
		t.Fatal("composite command must not inherit a single-segment prefix")
	}
	// Resource scope must match exactly.
	otherResources := []tool.Resource{
		{Kind: "file", Path: "bin/other", Access: tool.AccessWrite},
	}
	crossScope := shellGrantInvocation("5", "go build -o bin/app ./... -v", ".", otherResources...)
	if cache.MatchInvocation(crossScope, now) {
		t.Fatal("prefix crossed a resource scope boundary")
	}
	// Cwd scope must match exactly.
	otherCwd := shellGrantInvocation("6", "go build -o bin/app ./... -v", "./cmd", resources...)
	if cache.MatchInvocation(otherCwd, now) {
		t.Fatal("prefix crossed a cwd scope boundary")
	}
}

func TestApprovalCacheCompositeApprovalHasNoPrefix(t *testing.T) {
	now := time.Unix(5000, 0)
	cache := NewApprovalCache()
	resources := []tool.Resource{
		{Kind: "file", Path: "bin/app", Access: tool.AccessWrite},
	}
	composite := shellGrantInvocation(
		"1", "go build -o bin/app ./... && printf done", ".", resources...,
	)
	request, err := NewApprovalRequestForScope(composite, ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	if request.Grant.Prefix != nil {
		t.Fatalf("composite command got a prefix grant: %+v", request.Grant)
	}
	firstSegment := shellGrantInvocation("2", "go build -o bin/app ./...", ".", resources...)
	if cache.MatchInvocation(firstSegment, now) {
		t.Fatal("single segment inherited a composite approval")
	}
}
