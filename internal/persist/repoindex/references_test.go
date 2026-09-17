package repoindex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopedReferencesRefreshAndPrune(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "use.go", "package p\nfunc Use(){Run()}\n")
	writeFile(t, root, "run.go", "package p\nfunc Run(){}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := index.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	relations, err := store.ReferenceRelations(t.Context())
	if err != nil || len(relations) != 1 {
		t.Fatalf("relations=%+v err=%v", relations, err)
	}
	first := relations[0]
	writeFile(t, root, "use.go", "package p\nfunc Use(Run func()){Run()}\n")
	if _, err := index.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	relations, err = store.ReferenceRelations(t.Context())
	if err != nil || len(relations) != 0 {
		t.Fatalf("stale relation=%+v err=%v", relations, err)
	}
	sites, err := store.ReferenceSites(t.Context())
	if err != nil || len(sites["use.go"]) != 0 {
		t.Fatalf("stale sites=%+v err=%v", sites, err)
	}
	files, err := store.Files(t.Context())
	if err != nil || !files["use.go"].ScopeAware || files["use.go"].Digest == first.SourceDigest {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	edges, err := store.Edges(t.Context())
	if err != nil || len(edges) != 0 {
		t.Fatalf("legacy edge survived=%+v err=%v", edges, err)
	}
	if err := os.Remove(filepath.Join(root, "use.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	sites, err = store.ReferenceSites(t.Context())
	if err != nil || len(sites) != 0 {
		t.Fatalf("prune=%+v err=%v", sites, err)
	}
}

func TestReferenceSitesSurviveNewStoreAndReset(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "use.ts", "import {run} from './engine';run();")
	writeFile(t, root, "engine.ts", "export function run(){}")
	index, store := newIndex(t, root, Options{})
	if _, err := index.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.db, root)
	if err != nil {
		t.Fatal(err)
	}
	relations, err := reopened.ReferenceRelations(t.Context())
	if err != nil || len(relations) != 1 {
		t.Fatalf("persisted=%+v err=%v", relations, err)
	}
	if err := reopened.Reset(t.Context()); err != nil {
		t.Fatal(err)
	}
	sites, err := reopened.ReferenceSites(t.Context())
	if err != nil || len(sites) != 0 {
		t.Fatalf("reset=%+v err=%v", sites, err)
	}
}
