package state

import (
	"testing"
	"time"
)

func TestGuardianStartFencesDurableEventPublication(t *testing.T) {
	store, err := Open(t.Context(), Options{DataDir: t.TempDir(), BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(t.Context())
	if err := store.Append(t.Context(), testEvent(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.WithAuthorizationEvents(t.Context(), func() error {
		if store.authorizationEvents.TryRLock() {
			store.authorizationEvents.RUnlock()
			t.Fatal("event publication can race the physical start")
		}
		cursor, err := store.LastSequence(t.Context())
		if err != nil || cursor != 1 {
			t.Fatalf("fenced read: cursor=%d err=%v", cursor, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), testEvent(t, 2)); err != nil {
		t.Fatal(err)
	}
}
