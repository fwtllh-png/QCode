package wire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/joblog"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type childCloseBackend struct {
	sandbox.Backend
	close func() error
}

func (b childCloseBackend) Close() error { return b.close() }

type childCloseTool struct {
	permissionsSyncExecutor
	close func() error
}

func (t childCloseTool) Close() error { return t.close() }

func TestChildToolsetClosesProcessesBeforeLogsAndKeepsCloseErrors(t *testing.T) {
	child := &childToolset{resources: NewResourceStack()}
	if err := child.registerResourceClosers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.close(context.Background()) })
	// Populate fields after registration, as construction does.
	child.processes = process.NewSessionManager(0)
	logs, err := joblog.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child.jobLogs = logs
	child.processes.SetArchive(logs)
	var logsClosed atomic.Bool
	var order []string
	for i, entry := range child.resources.entries {
		child.resources.entries[i].close = func(ctx context.Context) error {
			err := entry.close(ctx)
			if entry.name == "job-logs" {
				logsClosed.Store(true)
			}
			order = append(order, entry.name)
			return err
		}
	}
	var processCloses atomic.Int32
	id, err := child.processes.Create(t.Context(), process.SessionOptions{
		Command: "read value", Dir: t.TempDir(), ThreadID: "child",
		OnClose: func(string) {
			processCloses.Add(1)
			if logsClosed.Load() {
				t.Error("job logs closed before the process")
			}
			if err := logs.Append("shutdown", []byte("process stopped")); err != nil {
				t.Error(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryFailure, backendFailure := errors.New("registry close"), errors.New("backend close")
	var registryCloses, backendCloses atomic.Int32
	child.registry = tool.NewRegistry(nil, nil)
	if err := child.registry.Register(childCloseTool{close: func() error {
		registryCloses.Add(1)
		if _, err := child.processes.Read(id, "child", 0); err == nil {
			t.Error("process remains registered during tool shutdown")
		}
		return registryFailure
	}}); err != nil {
		t.Fatal(err)
	}
	child.backend = childCloseBackend{close: func() error {
		backendCloses.Add(1)
		return backendFailure
	}}
	for range 2 {
		err := child.close(t.Context())
		if !errors.Is(err, registryFailure) || !errors.Is(err, backendFailure) {
			t.Fatalf("cleanup errors = %v", err)
		}
	}
	if processCloses.Load() != 1 || registryCloses.Load() != 1 || backendCloses.Load() != 1 {
		t.Fatalf("close counts: process=%d registry=%d backend=%d", processCloses.Load(), registryCloses.Load(), backendCloses.Load())
	}
	want := []string{"processes", "job-logs", "workspace-journal", "registry", "sandbox"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
	data, _, err := logs.Range("shutdown", 0, 0)
	if err != nil || string(data) != "process stopped" {
		t.Fatalf("shutdown log = %q, %v", data, err)
	}
}

func TestChildToolsetPartialCleanup(t *testing.T) {
	for _, stage := range []string{"empty", "sandbox", "registry"} {
		t.Run(stage, func(t *testing.T) {
			child := &childToolset{resources: NewResourceStack()}
			if err := child.registerResourceClosers(); err != nil {
				t.Fatal(err)
			}
			var closed []string
			if stage != "empty" {
				child.backend = childCloseBackend{close: func() error { closed = append(closed, "sandbox"); return nil }}
			}
			if stage == "registry" {
				child.registry = tool.NewRegistry(nil, nil)
				if err := child.registry.Register(childCloseTool{close: func() error { closed = append(closed, "registry"); return nil }}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := child.close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string][]string{"empty": nil, "sandbox": {"sandbox"}, "registry": {"registry", "sandbox"}}[stage]
			if !reflect.DeepEqual(closed, want) {
				t.Fatalf("closed = %v, want %v", closed, want)
			}
		})
	}
}

func TestChildToolsetFailedOpenCanRetryWithoutOwningParentResources(t *testing.T) {
	root := t.TempDir()
	content := contentstore.NewMemory(contentstore.Options{})
	t.Cleanup(func() { _ = content.Close(context.Background()) })
	if err := content.Put(t.Context(), "parent", []byte("retained")); err != nil {
		t.Fatal(err)
	}
	paths := childSkillPaths(t, root)
	stateParent := filepath.Dir(paths.SkillsStatePath)
	if err := os.WriteFile(stateParent, []byte("blocks skill state directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolsets := newChildToolsets(content, webtool.Options{}, config.Verify{}, config.Journal{}, nil, nil, nil, "", 0, t.TempDir(), paths)
	var parentCloses atomic.Int32
	toolsets.bindParentSandbox(childCloseBackend{close: func() error { parentCloses.Add(1); return nil }})
	t.Cleanup(func() { _ = toolsets.closeAll(context.Background()) })
	if child, err := toolsets.open(root, false); child != nil || err == nil || !strings.Contains(err.Error(), "child skills") {
		t.Fatalf("failed construction = %v, %v", child, err)
	}
	if len(toolsets.built) != 0 {
		t.Fatal("failed construction retained a toolset")
	}
	if err := os.Remove(stateParent); err != nil {
		t.Fatal(err)
	}
	child, err := toolsets.open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if reused, err := toolsets.open(root, false); err != nil || reused != child {
		t.Fatalf("toolset not retained for reuse: %v", err)
	}
	var childCloses atomic.Int32
	backend := child.backend
	child.backend = childCloseBackend{Backend: backend, close: func() error {
		childCloses.Add(1)
		return sandbox.CloseBackend(backend)
	}}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { toolsets.Release(root) })
	}
	group.Wait()
	if err := child.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if childCloses.Load() != 1 || parentCloses.Load() != 0 {
		t.Fatalf("close ownership: child=%d parent=%d", childCloses.Load(), parentCloses.Load())
	}
	if value, err := content.Get(t.Context(), "parent"); err != nil || string(value) != "retained" {
		t.Fatalf("parent content was closed: %q, %v", value, err)
	}
}

func TestSessionCloseReportsAllChildToolsetFailures(t *testing.T) {
	failure := errors.New("child close failed")
	toolsets := &childToolsets{built: make(map[string]*childToolset)}
	var closed atomic.Int32
	for _, root := range []string{"first", "second"} {
		child := &childToolset{resources: NewResourceStack()}
		if err := child.resources.Add("sandbox", func(context.Context) error {
			closed.Add(1)
			return failure
		}); err != nil {
			t.Fatal(err)
		}
		toolsets.built[filepath.Join("/children", root)] = child
	}
	session := &Session{
		resourceBundle:      resourceBundle{resources: NewResourceStack()},
		orchestrationBundle: orchestrationBundle{childTools: toolsets},
	}
	if err := session.registerResourceClosers(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := session.Close(t.Context())
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "/children/first") || !strings.Contains(err.Error(), "/children/second") {
			t.Fatalf("Session.Close = %v", err)
		}
	}
	if closed.Load() != 2 || len(toolsets.built) != 0 {
		t.Fatalf("child cleanup: closed=%d retained=%d", closed.Load(), len(toolsets.built))
	}
}
