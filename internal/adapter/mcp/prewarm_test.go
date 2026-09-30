package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestPrewarmOutlivesTheContextThatStartedIt(t *testing.T) {
	prewarm := NewPrewarm(NewPool(nil), Config{})
	prewarm.loadConfig = nil
	request, cancel := context.WithCancel(context.Background())
	prewarm.Start(request)
	t.Cleanup(prewarm.Stop)
	cancel()

	prewarm.RequestRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for prewarm.dirty.Load() {
		if time.Now().After(deadline) {
			t.Fatal("prewarm worker stopped with the request that constructed it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrewarmDefersAdapterAndPublishesSnapshotOnRefresh(t *testing.T) {
	connects := make(map[string]int)
	pool := NewPool(func(_ context.Context, name string, _ ServerConfig) (Transport, error) {
		connects[name]++
		return &healthFixtureTransport{toolName: "echo"}, nil
	})
	t.Cleanup(func() { _ = pool.ShutdownAll(context.Background()) })
	config := Config{Version: ConfigVersion, Servers: map[string]ServerConfig{
		"remote": fixtureServerConfig("echo"),
		"other":  fixtureServerConfig("echo"),
	}}
	registry := tool.NewRegistry(nil, nil)
	prewarm := NewPrewarm(pool, config)
	prewarm.SetRegistry(registry)
	prewarm.RequestRefresh()
	delete(config.Servers, "remote")
	if prewarm.adapter != nil || prewarm.cancel != nil || len(connects) != 0 {
		t.Fatal("prewarm constructor or refresh request started adapter work")
	}
	if err := prewarm.RefreshNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if connects["remote"] != 1 || len(registry.SourceRegistrations("mcp:remote")) != 1 {
		t.Fatalf("refresh connects=%v, registrations=%v", connects, registry.SourceRegistrations("mcp:remote"))
	}
	if err := prewarm.DisableServerPrefix(t.Context(), "remote"); err != nil {
		t.Fatal(err)
	}
	if err := prewarm.SyncCatalog(); err != nil {
		t.Fatal(err)
	}
	if len(registry.SourceRegistrations("mcp:remote")) != 0 {
		t.Fatal("catalog sync retained the disabled server")
	}
	if err := prewarm.RefreshNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if connects["remote"] != 1 {
		t.Fatal("refresh reconnected a blocked server")
	}
	prewarm.SetServerPrefixEnabled("remote", true)
	if err := prewarm.RefreshNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if connects["remote"] != 2 || len(registry.SourceRegistrations("mcp:remote")) != 1 {
		t.Fatal("refresh did not restore the enabled server from its config snapshot")
	}
}
