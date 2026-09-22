// Package persistence owns durable Runtime repositories.
package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	threadstate "github.com/fwtllh-png/QCode/internal/host/runtimeapi/thread"
	tracestate "github.com/fwtllh-png/QCode/internal/observability/trace"
	usagestate "github.com/fwtllh-png/QCode/internal/observability/usage"
	"github.com/fwtllh-png/QCode/internal/persist/agentpreset"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	snapshotstate "github.com/fwtllh-png/QCode/internal/persist/snapshot"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type PersistentRepositories struct {
	Sessions  *sessionstate.Repository
	Threads   *threadstate.Repository
	Lifecycle *threadstate.Lifecycle
	Snapshots *snapshotstate.Repository
	Usage     *usagestate.Repository
	Trace     *tracestate.Repository
}

func NewPersistentRepositories(
	store *state.Store,
	workspaceRoot ...string,
) (PersistentRepositories, error) {
	if store == nil {
		return PersistentRepositories{}, errors.New("persistent state store is required")
	}
	lifecycle := threadstate.NewLifecycle(store)
	if len(workspaceRoot) != 0 {
		lifecycle = threadstate.NewWorkspaceLifecycle(store, workspaceRoot[0])
	}
	return PersistentRepositories{
		Sessions:  sessionstate.NewSQLiteRepository(store.SQLite()).WithMaintenance(store.Maintain),
		Threads:   threadstate.NewSQLiteRepository(store.SQLite()),
		Lifecycle: lifecycle,
		Snapshots: snapshotstate.NewSQLiteRepository(store.SQLite(), store.Content()),
		Usage:     usagestate.NewSQLiteRepository(store.SQLite()),
		Trace:     tracestate.NewSQLiteRepository(store.SQLite()),
	}, nil
}

func OpenAgentPresetStore(
	store *state.Store,
	workspaceRoot string,
) (*agentpreset.Store, error) {
	if store == nil {
		return nil, errors.New("persistent state store is required")
	}
	digest := sha256.Sum256([]byte(filepath.Clean(workspaceRoot)))
	workspaceID := hex.EncodeToString(digest[:8])
	return agentpreset.Open(filepath.Join(
		store.Root(),
		"agent-presets",
		workspaceID,
		agentpreset.FileName,
	))
}

// EnsureThread creates workspace/session/thread seed rows when missing.
func EnsureThread(
	ctx context.Context,
	store *state.Store,
	threadID protocol.ThreadID,
	sessionID, workspaceRoot string,
) error {
	if store == nil || threadID == "" {
		return errors.New("persistent state store and thread id are required")
	}
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		return err
	}
	if _, err := repositories.Threads.Get(ctx, threadID); err == nil {
		return nil
	} else if !errors.Is(err, threadstate.ErrNotFound) {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = "session-local"
	}
	absRoot, err := sessionstate.NormalizeWorkspaceRoot(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	if _, err := repositories.Sessions.Get(ctx, sessionID); err == nil {
		_, err = repositories.Threads.Create(ctx, threadstate.Thread{
			ID: threadID, SessionID: sessionID, Title: "exec",
		})
		return err
	} else if !errors.Is(err, sessionstate.ErrNotFound) {
		return err
	}
	if err := repositories.Sessions.EnsureSeed(ctx, sessionID, absRoot); err != nil {
		return err
	}
	_, err = repositories.Threads.Create(ctx, threadstate.Thread{
		ID: threadID, SessionID: sessionID, Title: "exec",
	})
	return err
}

// EnsureChildThread binds a child thread to an existing user session. It must
// never synthesize a session because child agents are not top-level sessions.
func EnsureChildThread(
	ctx context.Context,
	store *state.Store,
	threadID protocol.ThreadID,
	sessionID string,
	parentThreadID protocol.ThreadID,
) error {
	if store == nil || threadID == "" || strings.TrimSpace(sessionID) == "" ||
		parentThreadID == "" {
		return errors.New("child thread, parent thread, session, and store are required")
	}
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		return err
	}
	if existing, getErr := repositories.Threads.Get(ctx, threadID); getErr == nil {
		if existing.SessionID != sessionID {
			return fmt.Errorf(
				"child thread %s belongs to session %s, not %s",
				threadID,
				existing.SessionID,
				sessionID,
			)
		}
		return nil
	} else if !errors.Is(getErr, threadstate.ErrNotFound) {
		return getErr
	}
	if _, err := repositories.Sessions.Get(ctx, sessionID); err != nil {
		return fmt.Errorf("load child parent session %s: %w", sessionID, err)
	}
	parent, err := repositories.Threads.Get(ctx, parentThreadID)
	if err != nil {
		return fmt.Errorf("load child parent thread %s: %w", parentThreadID, err)
	}
	if parent.SessionID != sessionID {
		return fmt.Errorf(
			"child parent thread %s belongs to session %s, not %s",
			parentThreadID,
			parent.SessionID,
			sessionID,
		)
	}
	_, err = repositories.Threads.Create(ctx, threadstate.Thread{
		ID:             threadID,
		SessionID:      sessionID,
		ParentThreadID: parentThreadID,
		Title:          "subagent",
	})
	return err
}
