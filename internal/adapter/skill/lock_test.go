package skill

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLockStoreConcurrentWritesRemainDecodable(t *testing.T) {
	store, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	lockfile := Lockfile{
		SchemaVersion: LockSchemaV1, RuntimeVersion: "1.0.0",
		Skills: []LockEntry{{
			Name: "review", Version: "1.0.0", Source: SourceConfigured,
			Digest: strings.Repeat("a", 64),
		}},
	}
	const count = 32
	var wait sync.WaitGroup
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := store.Write(lockfile); err != nil {
				t.Errorf("Write(): %v", err)
			}
			if _, err := store.Read(); err != nil {
				t.Errorf("Read(): %v", err)
			}
		}()
	}
	wait.Wait()
	if _, err := store.Read(); err != nil {
		t.Fatal(err)
	}
}

func TestLockStoreRejectsUnknownFieldsAndSymlinkParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills.lock.json")
	store, err := NewLockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{
		"schema_version": 1,
		"runtime_version": "1.0.0",
		"skills": [],
		"unknown": true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("lock with unknown field was accepted")
	}
	realParent := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realParent, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLockStore(filepath.Join(linkRoot, "lock.json")); err == nil {
		t.Fatal("lock store accepted symlink parent")
	}
}
