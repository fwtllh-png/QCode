package wire

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"

	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
)

func TestRepositoriesOwnDurableStores(t *testing.T) {
	if _, err := apppersistence.NewPersistentRepositories(nil); err == nil {
		t.Fatal("nil state store succeeded")
	}
	store, err := state.Open(t.Context(), state.Options{
		DataDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	if repositories.Sessions == nil || repositories.Threads == nil ||
		repositories.Lifecycle == nil || repositories.Snapshots == nil ||
		repositories.Usage == nil ||
		repositories.Trace == nil {
		t.Fatalf("incomplete repositories: %+v", repositories)
	}
}

func TestPersistentChildRegistrarLeavesHostSeededThreadToHost(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{
		DataDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	manager := app.NewThreadManager(nil)
	manager.SetChildFactory(func(app.ChildSpec) (*app.EngineAdapter, error) {
		return nil, errors.New("engine construction is not expected")
	})
	configureErr := ConfigurePersistentSubagents(
		manager, store, t.TempDir(), "process-session", nil,
		func(any) error { return nil },
	)
	if configureErr != nil {
		t.Fatal(configureErr)
	}
	registerErr := manager.RegisterChild("thread-host", app.ChildSpec{
		Workspace: t.TempDir(), HostSeeded: true,
	})
	if registerErr != nil {
		t.Fatal(registerErr)
	}
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	_, lookupErr := repositories.Threads.Get(
		t.Context(), "thread-host",
	)
	if !errors.Is(lookupErr, threadstate.ErrNotFound) {
		t.Fatalf("host-seeded thread lookup error = %v, want not found", lookupErr)
	}
	_, createErr := repositories.Threads.CreateSeed(
		t.Context(),
		sessionstate.Workspace{ID: "workspace-host", RootPath: t.TempDir()},
		sessionstate.Session{ID: "session-host", WorkspaceID: "workspace-host"},
		threadstate.Thread{ID: "thread-host", SessionID: "session-host"},
	)
	if createErr != nil {
		t.Fatalf("Host CreateSeed after child registration: %v", createErr)
	}
}

func TestPersistentChildRegistrarUsesOwningUserSession(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{
		DataDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if _, err := repositories.Threads.CreateSeed(
		t.Context(),
		sessionstate.Workspace{ID: "workspace-host", RootPath: workspace},
		sessionstate.Session{ID: "session-host", WorkspaceID: "workspace-host"},
		threadstate.Thread{ID: "thread-host", SessionID: "session-host"},
	); err != nil {
		t.Fatal(err)
	}
	manager := app.NewThreadManager(nil)
	manager.SetChildFactory(func(app.ChildSpec) (*app.EngineAdapter, error) {
		return nil, errors.New("engine construction is not expected")
	})
	if err := ConfigurePersistentSubagents(
		manager,
		store,
		workspace,
		"process-session",
		nil,
		func(any) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterChild("thread-agent", app.ChildSpec{
		SessionID:      "session-host",
		ParentThreadID: "thread-host",
		Workspace:      workspace,
	}); err != nil {
		t.Fatal(err)
	}
	child, err := repositories.Threads.Get(t.Context(), "thread-agent")
	if err != nil {
		t.Fatal(err)
	}
	if child.SessionID != "session-host" ||
		child.ParentThreadID != "thread-host" {
		t.Fatalf("child thread ownership = %+v", child)
	}
	if _, err := repositories.Sessions.Get(
		t.Context(),
		"process-session",
	); !errors.Is(err, sessionstate.ErrNotFound) {
		t.Fatalf("internal process session was persisted: %v", err)
	}
}
