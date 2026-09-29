// Package intergration_test verifies shared Runtime and Web host behavior.
// All scenarios and drivers are compiled only by go test.
package intergration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	webhost "github.com/fwtllh-png/QCode/internal/host"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// contractReceipt is what a host answers when it accepts an operation. Every transport
// has to name the thread, turn and item it assigned, because a client that cannot
// name the turn cannot cancel it or answer its approvals.
type contractReceipt struct {
	ThreadID protocol.ThreadID
	TurnID   protocol.TurnID
	ItemID   protocol.ItemID
}

// contractRefusal is a host declining an operation.
//
// A driver translates its wire refusal into a protocol error code. The
// translation is mechanical on purpose so the contract checks runtime meaning,
// not transport-specific framing.
type contractRefusal struct {
	Code    protocol.ErrorCode
	Message string
	// Retryable is what the host told the client about trying again. A transport
	// that says "retry" about something that can never succeed sends the client
	// into a loop, so it is part of the contract rather than a detail.
	Retryable bool
}

type contractReadState struct {
	Threads []contractThread
	Thread  contractThread
	Agents  []webhost.Agent
	Usage   []webhost.Usage
	Rollup  webhost.UsageRollup
}

type contractThread struct {
	ID             protocol.ThreadID
	SessionID      string
	ParentThreadID protocol.ThreadID
	Title          string
	Status         threadstate.ThreadStatus
	LatestSequence protocol.Cursor
	SourceCursor   protocol.Cursor
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Turns          []contractTurn
}

type contractTurn struct {
	ID          protocol.TurnID
	ThreadID    protocol.ThreadID
	OperationID protocol.OperationID
	Ordinal     uint64
	Status      threadstate.TurnStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

func (r *contractRefusal) Error() string {
	return string(r.Code) + ": " + r.Message
}

// hostSetup is what a scenario needs from the host before it starts.
type hostSetup struct {
	// Fixture is a provider fixture directory. Every scenario names one, because
	// what the model does is what the scenario is about.
	Fixture string
	// Prompt is what the fixture is waiting to be asked. A fixture rejects any
	// other prompt, so it belongs next to the fixture rather than in a scenario.
	Prompt string
	// Workspace, Tools and RepositoryRules are for scenarios that need a real tool
	// call — an approval only exists if something asked to write.
	Workspace         string
	WorkspaceIdentity *protocol.WorkspaceIdentity
	Tools             bool
	RepositoryRules   string
	MCPConfig         []byte
	MaxSteps          int
}

func (s hostSetup) writeMCPConfig(t *testing.T, stateRoot string) string {
	t.Helper()
	if len(s.MCPConfig) == 0 {
		return ""
	}
	path := filepath.Join(stateRoot, "mcp.json")
	if err := os.WriteFile(path, s.MCPConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostDriver is one transport under test, in protocol vocabulary. A scenario written
// against this cannot accidentally depend on a route shape or an RPC method name.
type hostDriver interface {
	// Transport names the envelope, for failure messages.
	Transport() string
	StartTurn(ctx context.Context, prompt string) (contractReceipt, error)
	StartTurnWithContext(
		ctx context.Context,
		prompt string,
		workspaceIdentity *protocol.WorkspaceIdentity,
		editorContext []protocol.EditorContextReference,
	) (contractReceipt, error)
	Cancel(ctx context.Context, turn contractReceipt, reason string) (contractReceipt, error)
	Decide(
		ctx context.Context,
		turn contractReceipt,
		requestID string,
		decision protocol.ApprovalDecision,
		planID string,
	) (contractReceipt, error)
	ReplyInput(
		ctx context.Context,
		turn contractReceipt,
		requestID, answer string,
		values map[string]string,
	) (contractReceipt, error)
	RecoverTurn(
		ctx context.Context,
		sourceTurnID protocol.TurnID,
		action protocol.TurnRecoveryAction,
		prompt string,
	) (contractReceipt, error)
	// Live delivers events as a client watching this host sees them, starting
	// after since. The channel closes when the host stops.
	Live(ctx context.Context, since protocol.Cursor) (<-chan protocol.Event, error)
	// History returns persisted events after since, the way a client that
	// reconnected with a stored cursor reads them.
	History(ctx context.Context, since protocol.Cursor, limit int) ([]protocol.Event, error)
	ReadState(ctx context.Context) (contractReadState, error)
	SessionProfile(ctx context.Context) (protocol.SessionProfileSnapshot, error)
	SessionToolCatalog(ctx context.Context) (protocol.SessionToolCatalog, error)
	ListSessions(
		ctx context.Context,
		query protocol.SessionListQuery,
	) (protocol.SessionList, error)
	UpdateSessionLifecycle(
		ctx context.Context,
		expectedRevision uint64,
		patch protocol.SessionLifecyclePatch,
	) (protocol.SessionLifecycleUpdate, error)
	DeleteSession(
		ctx context.Context,
		expectedRevision uint64,
	) (protocol.SessionDeleteResult, error)
	ListCheckpoints(
		ctx context.Context,
		limit int,
	) (protocol.CheckpointList, error)
	RestoreCheckpoint(
		ctx context.Context,
		checkpointID string,
	) (protocol.CheckpointRestoreResult, error)
	ForkCheckpoint(
		ctx context.Context,
		checkpointID, title string,
	) (protocol.CheckpointForkResult, error)
	UpdateSessionProfile(
		ctx context.Context,
		expectedRevision uint64,
		patch protocol.SessionProfilePatch,
	) (protocol.SessionProfileUpdateResult, error)
}

// hostFactory builds a host for one scenario. It registers its own cleanup.
type hostFactory func(t *testing.T, setup hostSetup) hostDriver

// hostScenario is one behaviour, plus what the host needs to show it.
type hostScenario struct {
	Name string
	// Setup runs per scenario so temporary directories are not shared.
	Setup func(t *testing.T) hostSetup
	Run   func(t *testing.T, host hostDriver, setup hostSetup)
}

// runContract executes every scenario against one transport.
func runContract(t *testing.T, newHost hostFactory) {
	t.Helper()
	for _, scenario := range scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			setup := scenario.Setup(t)
			host := newHost(t, setup)
			scenario.Run(t, host, setup)
		})
	}
}

// waitTimeout bounds one scenario's wait for a fixture-driven event.
const waitTimeout = 20 * time.Second

// terminal reports whether kind ends a turn. Exactly one of these per turn is a
// protocol invariant, and it is the invariant clients build their state on.
func terminal(kind protocol.EventKind) bool {
	switch kind {
	case protocol.EventTurnCompleted, protocol.EventTurnFailed, protocol.EventTurnCanceled:
		return true
	default:
		return false
	}
}

// collectUntilTerminal reads events for one turn until it ends, returning them in
// arrival order. Events for other turns are ignored: a host may be running more
// than the one thing a scenario cares about.
func collectUntilTerminal(
	t *testing.T,
	host hostDriver,
	events <-chan protocol.Event,
	turn protocol.TurnID,
) []protocol.Event {
	t.Helper()
	deadline := time.After(waitTimeout)
	var seen []protocol.Event
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("%s: event stream closed before turn %s ended; saw %s",
					host.Transport(), turn, kindsOf(seen))
			}
			if event.TurnID != turn {
				continue
			}
			seen = append(seen, event)
			if terminal(event.Kind) {
				return seen
			}
			if event.Kind == protocol.EventOperationRejected {
				t.Fatalf(
					"%s: operation was rejected while waiting for terminal: %+v",
					host.Transport(),
					event.Data,
				)
			}
		case <-deadline:
			t.Fatalf("%s: turn %s did not end within %s; saw %s",
				host.Transport(), turn, waitTimeout, kindsOf(seen))
		}
	}
}

// waitForKind reads until one event of kind arrives for turn.
func waitForKind(
	t *testing.T,
	host hostDriver,
	events <-chan protocol.Event,
	turn protocol.TurnID,
	kind protocol.EventKind,
) protocol.Event {
	t.Helper()
	deadline := time.After(waitTimeout)
	var seen []protocol.Event
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("%s: event stream closed before %s; saw %s",
					host.Transport(), kind, kindsOf(seen))
			}
			if turn != "" && event.TurnID != turn {
				continue
			}
			seen = append(seen, event)
			if event.Kind == kind {
				return event
			}
			if event.Kind == protocol.EventOperationRejected {
				t.Fatalf(
					"%s: operation was rejected before %s: %+v",
					host.Transport(),
					kind,
					event.Data,
				)
			}
			if terminal(event.Kind) {
				t.Fatalf("%s: turn ended with %s before %s arrived; saw %s",
					host.Transport(), event.Kind, kind, kindsOf(seen))
			}
		case <-deadline:
			t.Fatalf("%s: %s did not arrive within %s; saw %s",
				host.Transport(), kind, waitTimeout, kindsOf(seen))
		}
	}
}

func kindsOf(events []protocol.Event) []protocol.EventKind {
	kinds := make([]protocol.EventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func countTerminals(events []protocol.Event) int {
	count := 0
	for _, event := range events {
		if terminal(event.Kind) {
			count++
		}
	}
	return count
}
