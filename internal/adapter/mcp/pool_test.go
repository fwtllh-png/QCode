package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPoolIsolatesFailedServer(t *testing.T) {
	healthy := &healthFixtureTransport{toolName: "echo"}
	pool := NewPool(func(
		_ context.Context,
		name string,
		_ ServerConfig,
	) (Transport, error) {
		if name == "broken" {
			return nil, errors.New("cannot connect")
		}
		return healthy, nil
	})
	config := Config{Version: ConfigVersion, Servers: map[string]ServerConfig{
		"healthy": fixtureServerConfig("echo"),
		"broken":  fixtureServerConfig("echo"),
	}}
	if changed, err := pool.Reload(t.Context(), config); err != nil || !changed {
		t.Fatalf("reload changed=%v err=%v", changed, err)
	}
	catalog := pool.Catalog()
	if len(catalog) != 1 || catalog[0].Server != "healthy" {
		t.Fatalf("catalog = %+v", catalog)
	}
	health := healthByServer(pool.HealthSnapshots())
	if health["healthy"].State != HealthHealthy || health["broken"].State != HealthOpen {
		t.Fatalf("health = %+v", health)
	}
}

func TestPoolReloadReconnectsOnlyChangedServer(t *testing.T) {
	var mu sync.Mutex
	connects := map[string]int{}
	pool := NewPool(func(
		_ context.Context,
		name string,
		_ ServerConfig,
	) (Transport, error) {
		mu.Lock()
		connects[name]++
		mu.Unlock()
		return &healthFixtureTransport{toolName: "echo"}, nil
	})
	config := Config{Version: ConfigVersion, Servers: map[string]ServerConfig{
		"alpha": fixtureServerConfig("echo"),
		"beta":  fixtureServerConfig("echo"),
	}}
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	alpha := config.Servers["alpha"]
	alpha.Args = []string{"changed"}
	config.Servers["alpha"] = alpha
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if connects["alpha"] != 2 || connects["beta"] != 1 {
		t.Fatalf("connects = %v", connects)
	}
}

func fixtureServerConfig(toolName string) ServerConfig {
	return ServerConfig{
		Transport: "stdio", Command: "fixture", HostTrusted: true,
		Tools: map[string]ToolBinding{
			toolName: {
				Capability: "read", AccessMode: "read",
				ParallelPolicy: "concurrent", SandboxRequirement: "none",
			},
		},
		ConnectTimeout: time.Second, CallTimeout: time.Second,
		ShutdownTimeout: time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			FailureThreshold: 3, Cooldown: time.Second,
		},
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
