package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const workspaceReplayPageSize = 512

// WorkspaceEventStore is a non-owning event projection for one Runtime.
type WorkspaceEventStore struct {
	store         *Store
	workspaceRoot string
}

func NewWorkspaceEventStore(
	store *Store,
	workspaceRoot string,
) *WorkspaceEventStore {
	return &WorkspaceEventStore{store: store, workspaceRoot: workspaceRoot}
}

func (s *WorkspaceEventStore) Append(
	ctx context.Context,
	event protocol.Event,
) error {
	if s.workspaceRoot == "" {
		return s.store.Append(ctx, event)
	}
	belongs, err := s.store.EventBelongsToWorkspace(
		ctx,
		event,
		s.workspaceRoot,
	)
	if err != nil {
		return err
	}
	if !belongs {
		return errors.New("event does not belong to the Runtime Workspace")
	}
	return s.store.Append(ctx, event)
}

func (s *WorkspaceEventStore) Replay(
	ctx context.Context,
	cursor protocol.Cursor,
) ([]protocol.Event, error) {
	if s.workspaceRoot == "" {
		return s.store.Replay(ctx, cursor)
	}
	return s.store.ReplayWorkspace(ctx, cursor, s.workspaceRoot)
}

func (s *WorkspaceEventStore) ReplayLimit(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) ([]protocol.Event, bool, error) {
	if s.workspaceRoot == "" {
		return s.store.ReplayLimit(ctx, cursor, limit)
	}
	return s.store.ReplayWorkspaceLimit(
		ctx,
		cursor,
		s.workspaceRoot,
		limit,
	)
}

func (s *WorkspaceEventStore) LastSequence(
	ctx context.Context,
) (protocol.Cursor, error) {
	return s.store.LastSequence(ctx)
}

// ReplaySessionBefore applies Workspace ownership after the log's session
// index. Declared agent roots retain precedence over their thread's root.
func (s *WorkspaceEventStore) ReplaySessionBefore(ctx context.Context, sessionID string, threadIDs []protocol.ThreadID, before, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, &workspaceReplayLimitError{}
	}
	if s.workspaceRoot == "" {
		return s.store.ReplaySessionBefore(ctx, sessionID, threadIDs, before, through, limit)
	}
	root := physicalWorkspaceRoot(s.workspaceRoot)
	threads, err := s.store.workspaceThreadIDs(ctx, root)
	if err != nil {
		return nil, false, err
	}
	result := make([]protocol.Event, 0, limit+1)
	for {
		page, more, err := s.store.ReplaySessionBefore(ctx, sessionID, threadIDs, before, through, limit+1-len(result))
		if err != nil {
			return nil, false, err
		}
		for _, event := range page {
			before = event.Sequence
			if workspaceOwnsEvent(root, threads, event) {
				result = append(result, event)
			}
		}
		if len(result) > limit {
			return result[:limit], true, nil
		}
		if !more {
			return result, false, nil
		}
	}
}

func (s *Store) ReplaySessionBefore(ctx context.Context, sessionID string, threads []protocol.ThreadID, before, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, false, ErrClosed
	}
	return s.events.ReplaySessionBefore(ctx, sessionID, threads, before, through, limit)
}

func (s *WorkspaceEventStore) EventByID(
	ctx context.Context,
	eventID protocol.EventID,
) (protocol.Event, bool, error) {
	event, found, err := s.store.EventByID(ctx, eventID)
	if err != nil || !found || s.workspaceRoot == "" {
		return event, found, err
	}
	belongs, err := s.store.EventBelongsToWorkspace(
		ctx,
		event,
		s.workspaceRoot,
	)
	if err != nil || !belongs {
		return protocol.Event{}, false, err
	}
	return event, true, nil
}

// Close is intentionally a no-op because the Supervisor owns the shared Store.
func (*WorkspaceEventStore) Close(context.Context) error { return nil }

// SharedContentStore keeps process-owned content open while one Runtime stops.
type SharedContentStore struct {
	*cas.Store
}

func NewSharedContentStore(store *cas.Store) *SharedContentStore {
	return &SharedContentStore{Store: store}
}

func (*SharedContentStore) Close(context.Context) error { return nil }

// EventBelongsToWorkspace resolves an event's durable Workspace owner.
func (s *Store) EventBelongsToWorkspace(
	ctx context.Context,
	event protocol.Event,
	workspaceRoot string,
) (bool, error) {
	workspaceRoot = physicalWorkspaceRoot(workspaceRoot)
	if declaredRoot := eventWorkspaceRoot(event.Data); declaredRoot != "" {
		return physicalWorkspaceRoot(declaredRoot) == workspaceRoot, nil
	}
	if event.ThreadID == "" {
		return false, nil
	}
	var found int
	err := s.sqlite.DB().QueryRowContext(ctx, `
		SELECT 1
		FROM threads AS t
		JOIN sessions AS session ON session.id = t.session_id
		JOIN workspaces AS workspace ON workspace.id = session.workspace_id
		WHERE t.id = ? AND workspace.root_path = ?`,
		event.ThreadID,
		workspaceRoot,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) workspaceThreadIDs(
	ctx context.Context,
	workspaceRoot string,
) (map[protocol.ThreadID]struct{}, error) {
	rows, err := s.sqlite.DB().QueryContext(ctx, `
		SELECT t.id
		FROM threads AS t
		JOIN sessions AS session ON session.id = t.session_id
		JOIN workspaces AS workspace ON workspace.id = session.workspace_id
		WHERE workspace.root_path = ?`,
		workspaceRoot,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[protocol.ThreadID]struct{})
	for rows.Next() {
		var threadID protocol.ThreadID
		if err := rows.Scan(&threadID); err != nil {
			return nil, err
		}
		result[threadID] = struct{}{}
	}
	return result, rows.Err()
}

func physicalWorkspaceRoot(root string) string {
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return filepath.Clean(resolved)
	}
	return root
}

// ReplayWorkspace returns committed events after cursor that belong to one
// Workspace while retaining the process-wide monotonic event sequence.
func (s *Store) ReplayWorkspace(
	ctx context.Context,
	cursor protocol.Cursor,
	workspaceRoot string,
) ([]protocol.Event, error) {
	workspaceRoot = physicalWorkspaceRoot(workspaceRoot)
	threads, err := s.workspaceThreadIDs(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	result := make([]protocol.Event, 0)
	for {
		page, more, err := s.ReplayLimit(ctx, cursor, workspaceReplayPageSize)
		if err != nil {
			return nil, err
		}
		for _, event := range page {
			cursor = event.Sequence
			if workspaceOwnsEvent(workspaceRoot, threads, event) {
				result = append(result, event)
			}
		}
		if !more {
			return result, nil
		}
	}
}

// ReplayWorkspaceLimit bounds decoded Workspace events even when the global
// event stream contains records owned by other Workspaces.
func (s *Store) ReplayWorkspaceLimit(
	ctx context.Context,
	cursor protocol.Cursor,
	workspaceRoot string,
	limit int,
) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, &workspaceReplayLimitError{}
	}
	workspaceRoot = physicalWorkspaceRoot(workspaceRoot)
	threads, err := s.workspaceThreadIDs(ctx, workspaceRoot)
	if err != nil {
		return nil, false, err
	}
	result := make([]protocol.Event, 0, limit+1)
	pageSize := max(workspaceReplayPageSize, limit+1)
	for {
		page, more, err := s.ReplayLimit(ctx, cursor, pageSize)
		if err != nil {
			return nil, false, err
		}
		for _, event := range page {
			cursor = event.Sequence
			if workspaceOwnsEvent(workspaceRoot, threads, event) {
				result = append(result, event)
				if len(result) > limit {
					return result[:limit], true, nil
				}
			}
		}
		if !more {
			return result, false, nil
		}
	}
}

func workspaceOwnsEvent(
	workspaceRoot string,
	threads map[protocol.ThreadID]struct{},
	event protocol.Event,
) bool {
	if declaredRoot := eventWorkspaceRoot(event.Data); declaredRoot != "" {
		return physicalWorkspaceRoot(declaredRoot) == workspaceRoot
	}
	_, found := threads[event.ThreadID]
	return found
}

type workspaceReplayLimitError struct{}

func (*workspaceReplayLimitError) Error() string {
	return "event replay limit must be positive"
}

func eventWorkspaceRoot(data protocol.EventData) string {
	switch value := data.(type) {
	case *protocol.AgentSpawnedData:
		return value.WorkspaceRoot
	case *protocol.AgentStatusData:
		return value.WorkspaceRoot
	case *protocol.AgentMessageData:
		return value.WorkspaceRoot
	case *protocol.AgentIntegrationData:
		return value.WorkspaceRoot
	default:
		return ""
	}
}
