//go:build darwin

package mcp

import (
	"context"
	"testing"
	"time"
)

func TestStdioForcedKillCleansProcessGroup(t *testing.T) {
	transport, err := NewAuthorizedStdioTransport(
		context.Background(),
		"fixture",
		ServerConfig{
			Command: "/bin/sh",
			Args:    []string{"-c", `trap '' TERM; while :; do :; done`},
		},
		testRuntimeAuthority(t, t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := transport.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.processDone:
	default:
		t.Fatal("hung MCP process was not reaped")
	}
}
