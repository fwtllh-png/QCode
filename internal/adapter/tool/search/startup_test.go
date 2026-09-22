package search

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
)

func TestRegisterSymbolToolsDuringIndexBuild(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "api.go"), "package api\nfunc Serve() {}\n")
	store, err := repoindex.NewStore(openIndexDatabase(t), root)
	if err != nil {
		t.Fatal(err)
	}
	walker, err := repowalk.New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	index, err := repoindex.NewIndex(store, walker, repoindex.Options{Now: func() time.Time {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return time.Now()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var registrationFinished chan struct{}
	go func() {
		defer close(finished)
		_, _ = index.Ensure(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		<-finished
		if registrationFinished != nil {
			<-registrationFinished
		}
	})
	select {
	case <-entered:
	case <-finished:
		t.Fatal("index build did not reach scan")
	case <-time.After(5 * time.Second):
		t.Fatal("index build did not start")
	}
	registry := tool.NewRegistry(nil, nil)
	registered := make(chan error, 1)
	registrationFinished = make(chan struct{})
	go func() {
		defer close(registrationFinished)
		registered <- RegisterWithIndex(registry, root, searchTestBackend{}, index)
	}()
	select {
	case err := <-registered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool registration waited for index construction")
	}
	for _, name := range []string{KindSymbol, KindDefinition, KindReferences, KindRelatedTests} {
		_, descriptor, _, err := registry.Resolve(name)
		if err != nil || descriptor.Availability != tool.AvailabilityAvailable {
			t.Fatalf("pending index tool %s: descriptor=%+v error=%v", name, descriptor, err)
		}
	}
}
