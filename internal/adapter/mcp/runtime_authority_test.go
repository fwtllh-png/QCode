package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
)

func testRuntimeAuthority(t *testing.T, workspace string) *RuntimeAuthority {
	t.Helper()
	runtimeAuthority, err := NewRuntimeAuthority(
		workspace, "", 1, nil, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeAuthority.RequireHostTrust = false
	return runtimeAuthority
}

func TestRuntimeAuthorityRequiresLeaseAuthority(t *testing.T) {
	if _, err := NewRuntimeAuthority(t.TempDir(), "", 1, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "lease authority is required") {
		t.Fatalf("nil lease authority error = %v, want lease authority is required", err)
	}
}

func TestStdioLifecycleBindsConfigGenerationAndTerminates(t *testing.T) {
	runtimeAuthority, err := NewRuntimeAuthority(
		t.TempDir(), "", 1, nil, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	config := ServerConfig{
		Transport: "stdio", HostTrusted: true, Command: "/bin/sh",
		Args:           []string{"-c", "while read line; do :; done"},
		ConnectTimeout: time.Second,
	}
	environment := []string{"PATH=/usr/bin:/bin"}
	untrusted := config
	untrusted.HostTrusted = false
	if _, err := runtimeAuthority.Start(
		t.Context(), "fixture", untrusted, environment,
	); err == nil {
		t.Fatal("untrusted MCP lifecycle was started")
	}
	revoked := config
	revoked.Authority = func(context.Context) error {
		return context.Canceled
	}
	if _, err := runtimeAuthority.Start(
		t.Context(), "fixture", revoked, environment,
	); err == nil {
		t.Fatal("revoked MCP lifecycle was started")
	}
	first, err := runtimeAuthority.Start(t.Context(), "fixture", config, environment)
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	terminal, err := first.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Lease.State != authority.LeaseSettled ||
		!terminal.Handle.Terminal {
		t.Fatalf("terminal lifecycle = %+v", terminal)
	}
	if _, err := first.Stdin(); err == nil {
		t.Fatal("terminal MCP lifecycle retained stdin authority")
	}

	config.Args = []string{"-c", "while read line; do printf '%s' \"$line\"; done"}
	second, err := runtimeAuthority.Start(t.Context(), "fixture", config, environment)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	secondSnapshot, err := second.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if secondSnapshot.Handle.Generation <= firstSnapshot.Handle.Generation ||
		secondSnapshot.Lease.SubjectDigest == firstSnapshot.Lease.SubjectDigest {
		t.Fatalf(
			"lifecycle identity did not advance: first=%+v second=%+v",
			firstSnapshot, secondSnapshot,
		)
	}
}
