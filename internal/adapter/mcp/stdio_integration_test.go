package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStdioConnectionRealFixtureLifecycle(t *testing.T) {
	binary := buildMCPFixture(t)
	transport, err := NewAuthorizedStdioTransport(
		context.Background(),
		"fixture",
		ServerConfig{
			Command: binary,
			Args:    []string{"--transport=stdio", "--stderr-bytes=65536"},
		},
		testRuntimeAuthority(t, t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := NewConnection("fixture", transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := connection.DiscoverTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "fixture.echo" {
		t.Fatalf("discovered tools = %+v", tools)
	}
	result, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{"value":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "fixture result" {
		t.Fatalf("tool result = %+v", result)
	}
	if len(transport.StderrTail()) > defaultStderrTailBytes {
		t.Fatalf("stderr tail bytes = %d", len(transport.StderrTail()))
	}

	callCtx, cancelCall := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = connection.CallTool(callCtx, "fixture.wait", json.RawMessage(`{}`))
	cancelCall()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
	if _, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := connection.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestStdioRejectsSecretEnvironment(t *testing.T) {
	_, err := NewAuthorizedStdioTransport(
		context.Background(),
		"fixture",
		ServerConfig{Command: "unused", Env: []string{"MCP_API_KEY=secret"}},
		testRuntimeAuthority(t, t.TempDir()),
	)
	if err == nil {
		t.Fatal("secret environment was accepted")
	}
}

func TestPoolHashReloadAndCatalog(t *testing.T) {
	binary := buildMCPFixture(t)
	config := Config{
		Version: ConfigVersion,
		Servers: map[string]ServerConfig{
			"fixture-server": {
				Transport: "stdio", HostTrusted: true,
				Command: binary,
				Args:    []string{"--transport=stdio"},
				Tools: map[string]ToolBinding{
					"fixture.echo": {
						Capability:         "read",
						AccessMode:         "read",
						ParallelPolicy:     "concurrent",
						SandboxRequirement: "none",
					},
				},
			},
		},
	}
	pool := NewPool(NewAuthorizedTransportFactory(
		testRuntimeAuthority(t, t.TempDir()),
	))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	changed, err := pool.Reload(ctx, config)
	if err != nil || !changed {
		t.Fatalf("first reload changed=%t err=%v", changed, err)
	}
	catalog := pool.Catalog()
	if len(catalog) != 1 || catalog[0].ModelName != "mcp_fixture_server_fixture_echo" {
		t.Fatalf("catalog = %+v", catalog)
	}
	changed, err = pool.Reload(ctx, config)
	if err != nil || changed {
		t.Fatalf("no-op reload changed=%t err=%v", changed, err)
	}
	if err := pool.ShutdownAll(ctx); err != nil {
		t.Fatal(err)
	}
}

func buildMCPFixture(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	name := "mcp-fixture"
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "./internal/adapter/mcp/testdata/fixture")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	return binary
}
