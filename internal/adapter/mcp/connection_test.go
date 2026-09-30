package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type paginatedTransport struct {
	listCalls int
}

type optionalCapabilityTransport struct {
	executeCalls atomic.Int64
}

func (t *optionalCapabilityTransport) Request(
	_ context.Context,
	method string,
	_ any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*InitializeResult) = InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities: map[string]any{
				"resources": map[string]any{},
				"prompts":   map[string]any{},
			},
			ServerInfo: ClientInfo{Name: "optional", Version: "1"},
		}
	case "resources/list", "resources/templates/list", "prompts/list":
		return &RPCError{Code: -32601, Message: "method not found"}
	default:
		t.executeCalls.Add(1)
	}
	return nil
}

func (*optionalCapabilityTransport) Notify(context.Context, string, any) error { return nil }
func (*optionalCapabilityTransport) Close(context.Context) error               { return nil }
func (*optionalCapabilityTransport) StderrTail() string                        { return "" }

func TestConnectionToleratesUnsupportedOptionalCapabilities(t *testing.T) {
	transport := &optionalCapabilityTransport{}
	connection, err := NewConnection("optional", transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	discovery, err := connection.DiscoverAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 0 ||
		len(discovery.Resources) != 0 ||
		len(discovery.Prompts) != 0 {
		t.Fatalf("unexpected discovery = %+v", discovery)
	}
	_, err = connection.ReadResource(context.Background(), "fixture://missing")
	if !errors.Is(err, ErrNotAdvertised) {
		t.Fatalf("read error = %v", err)
	}
	_, err = connection.GetPrompt(context.Background(), "missing", nil)
	if !errors.Is(err, ErrNotAdvertised) {
		t.Fatalf("prompt error = %v", err)
	}
	_, err = connection.CallTool(context.Background(), "missing", json.RawMessage(`{}`))
	if !errors.Is(err, ErrNotAdvertised) {
		t.Fatalf("tool error = %v", err)
	}
	if transport.executeCalls.Load() != 0 {
		t.Fatal("unadvertised catalog name reached MCP wire")
	}
}

func (t *paginatedTransport) Request(
	_ context.Context,
	method string,
	params any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*InitializeResult) = InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      ClientInfo{Name: "pages", Version: "1"},
		}
	case "tools/list":
		t.listCalls++
		cursor := params.(ListToolsParams).Cursor
		if cursor == "" {
			*target.(*ListToolsResult) = ListToolsResult{
				Tools: []Tool{{
					Name:        "first",
					InputSchema: map[string]any{"type": "object"},
				}},
				NextCursor: "page-2",
			}
		} else {
			*target.(*ListToolsResult) = ListToolsResult{
				Tools: []Tool{{
					Name:        "second",
					InputSchema: map[string]any{"type": "object"},
				}},
			}
		}
	}
	return nil
}

func (*paginatedTransport) Notify(context.Context, string, any) error { return nil }
func (*paginatedTransport) Close(context.Context) error               { return nil }
func (*paginatedTransport) StderrTail() string                        { return "" }

func TestConnectionDiscoversPaginatedTools(t *testing.T) {
	transport := &paginatedTransport{}
	connection, err := NewConnection("pages", transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := connection.DiscoverTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "first" || tools[1].Name != "second" ||
		transport.listCalls != 2 {
		t.Fatalf("tools=%+v list calls=%d", tools, transport.listCalls)
	}
}

// failingCloseTransport is a transport that fails on Close.
type failingCloseTransport struct {
	closed   bool
	closeErr error
}

func (t *failingCloseTransport) Request(ctx context.Context, method string, params any, result any) error {
	return errors.New("not implemented")
}

func (t *failingCloseTransport) Notify(ctx context.Context, method string, params any) error {
	return nil
}

func (t *failingCloseTransport) Close(ctx context.Context) error {
	t.closed = true
	return t.closeErr
}

func (t *failingCloseTransport) StderrTail() string { return "" }

// TestConnectionCloseFailureDoesNotLeakResources verifies that when a connection's
// Close fails, the transport is still closed and the connection is idempotent.
// This catches the bug where old connection close failure causes the transport
// to be leaked.
func TestConnectionCloseFailureDoesNotLeakResources(t *testing.T) {
	transport := &failingCloseTransport{
		closeErr: errors.New("transport close failed"),
	}

	conn, err := NewConnection("test-server", transport, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Attempt to close the connection — it should return the transport error.
	err = conn.Close(t.Context())
	if err == nil {
		t.Error("expected Close to return an error from the failing transport")
	}

	// The transport should have been closed (even if it returned an error).
	if !transport.closed {
		t.Error("BUG: transport.Close was not called; connection was leaked")
	}

	// Second close attempt should be idempotent (via sync.Once).
	// The first error is preserved and returned on subsequent calls.
	err2 := conn.Close(t.Context())
	t.Logf("first close error: %v, second close error: %v", err, err2)

	// The transport should only be closed once.
	transport.closed = false // Reset to verify it's not called again.
	_ = conn.Close(t.Context())
	if transport.closed {
		t.Error("BUG: transport.Close was called more than once (double-close)")
	}
}

// FuzzMCPConnectionCloseIsIdempotent verifies that calling Close() on a
// Connection multiple times (possibly with different contexts) is safe and
// does not panic, leak, or double-close the transport. This catches the
// bug where old connection close failure causes the transport to be leaked.
func FuzzMCPConnectionCloseIsIdempotent(f *testing.F) {
	f.Add(uint8(1), uint8(0))
	f.Add(uint8(5), uint8(1))
	f.Add(uint8(10), uint8(2))

	f.Fuzz(func(t *testing.T, closeCount uint8, failMode uint8) {
		closeCount = uint8(int(closeCount)%10 + 1)
		failMode = failMode % 3

		var closeCalls int
		var mu sync.Mutex

		transport := &fuzzTransport{
			closeFn: func() error {
				mu.Lock()
				closeCalls++
				count := closeCalls
				mu.Unlock()
				switch failMode {
				case 0:
					return nil
				case 1:
					return errors.New("simulated close failure")
				default:
					// First close fails, subsequent succeed.
					if count == 1 {
						return errors.New("simulated close failure")
					}
					return nil
				}
			},
		}

		conn, err := NewConnection("fuzz-server", transport, 0)
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		for i := uint8(0); i < closeCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = conn.Close(t.Context())
			}()
		}
		wg.Wait()

		mu.Lock()
		count := closeCalls
		mu.Unlock()

		// Transport Close should be called exactly once (sync.Once protection).
		if count != 1 {
			t.Errorf("transport.Close called %d times, expected exactly 1", count)
		}
	})
}

// fuzzTransport implements Transport for fuzz testing.
type fuzzTransport struct {
	closeFn func() error
}

func (t *fuzzTransport) Request(ctx context.Context, method string, params any, result any) error {
	return errors.New("not implemented")
}

func (t *fuzzTransport) Notify(ctx context.Context, method string, params any) error {
	return nil
}

func (t *fuzzTransport) Close(ctx context.Context) error {
	if t.closeFn != nil {
		return t.closeFn()
	}
	return nil
}

func (t *fuzzTransport) StderrTail() string { return "" }
