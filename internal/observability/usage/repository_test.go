package usage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestUsageProjectionIsTransactionalAndIdempotent(t *testing.T) {
	repository := testRepository(t)
	start := testEvent(t, 1, &protocol.TurnStartedData{Provider: "provider", Model: "model"})
	start.CreatedAt = time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	event := testEvent(t, 2, &protocol.UsageData{
		Sample: 1, InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2,
	})
	event.CreatedAt = start.CreatedAt.Add(time.Minute)
	if err := repository.Project(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err := repository.Project(t.Context(), event); err != nil {
		t.Fatalf("idempotent projection: %v", err)
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{
		SessionID: "session-1", TurnID: "turn-1", Provider: "provider", Model: "model",
		Start: start.CreatedAt, End: event.CreatedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("aggregates = %+v", aggregates)
	}
	got := aggregates[0]
	if got.Calls != 1 || got.InputTokens != 10 ||
		got.OutputTokens != 4 || got.ReasoningTokens != 2 {
		t.Fatalf("aggregate = %+v", got)
	}
	scoped, err := repository.QueryAggregates(t.Context(), Query{
		WorkspaceRoot: "/workspace", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 {
		t.Fatalf("workspace usage = %+v", scoped)
	}
	scoped, err = repository.QueryAggregates(t.Context(), Query{
		WorkspaceRoot: "/other", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 0 {
		t.Fatalf("foreign workspace usage = %+v", scoped)
	}
	if _, err := repository.QueryAggregates(t.Context(), Query{Limit: 1001}); err == nil {
		t.Fatal("oversized usage query succeeded")
	}
	outside, err := repository.QueryAggregates(t.Context(), Query{
		Start: event.CreatedAt.Add(time.Second), End: event.CreatedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) != 0 {
		t.Fatalf("out-of-range aggregates = %+v", outside)
	}

	conflict := event
	conflict.Data = &protocol.UsageData{Sample: 1, InputTokens: 99}
	if err := repository.Project(t.Context(), conflict); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("conflicting sequence error = %v", err)
	}
	aggregates, err = repository.QueryAggregates(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 || aggregates[0].InputTokens != 10 {
		t.Fatalf("conflict changed aggregate: %+v", aggregates)
	}
}

func TestUsageProjectsCachedTokensAndCost(t *testing.T) {
	repository := testRepository(t)
	start := testEvent(t, 1, &protocol.TurnStartedData{Provider: "provider", Model: "model"})
	start.CreatedAt = time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	event := testEvent(t, 2, &protocol.UsageData{
		Sample: 1, InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2,
		CachedTokens: 6, CostMicrounits: 1250, CostKnown: true,
	})
	event.CreatedAt = start.CreatedAt.Add(time.Minute)
	if err := repository.Project(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{TurnID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("aggregates = %+v", aggregates)
	}
	if got := aggregates[0]; got.CachedTokens != 6 || got.CostMicrounits != 1250 ||
		got.PricedCalls != 1 || got.UnpricedCalls != 0 {
		t.Fatalf("cached/cost aggregate = %+v", got)
	}
	// Same sequence with different cached/cost must be rejected, not silently kept.
	conflict := event
	conflict.Data = &protocol.UsageData{
		Sample: 1, InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2,
		CachedTokens: 6, CostMicrounits: 9999, CostKnown: true,
	}
	if err := repository.Project(t.Context(), conflict); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("cost conflict error = %v", err)
	}
}

func TestUsagePersistsPerCallModelMetadataProvenance(t *testing.T) {
	repository := testRepository(t)
	turnMetadata := testModelMetadata("bundled")
	start := testEvent(t, 1, &protocol.TurnStartedData{
		Provider: "provider", Model: "model", ModelMetadata: turnMetadata,
	})
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	callMetadata := testModelMetadata("provider_discovery")
	for _, event := range []protocol.Event{
		testEvent(t, 2, &protocol.UsageData{
			Sample: 1, InputTokens: 10,
		}),
		testEvent(t, 3, &protocol.UsageData{
			Sample: 2, Provider: "summary-provider", Model: "summary-model",
			ModelMetadata: callMetadata, InputTokens: 20,
		}),
	} {
		if err := repository.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := repository.QueryAggregates(
		t.Context(),
		Query{TurnID: "turn-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 2 {
		t.Fatalf("aggregates = %+v", aggregates)
	}
	got := map[string]*protocol.ModelMetadataProvenance{}
	for _, aggregate := range aggregates {
		got[aggregate.Model] = aggregate.ModelMetadata
	}
	if got["model"] == nil || got["model"].Limits != "bundled" {
		t.Fatalf("turn metadata = %+v", got["model"])
	}
	if got["summary-model"] == nil ||
		got["summary-model"].Limits != "provider_discovery" {
		t.Fatalf("call metadata = %+v", got["summary-model"])
	}

	conflict := testEvent(t, 3, &protocol.UsageData{
		Sample: 2, Provider: "summary-provider", Model: "summary-model",
		ModelMetadata: testModelMetadata("operator_config"), InputTokens: 20,
	})
	if err := repository.Project(
		t.Context(),
		conflict,
	); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("metadata conflict error = %v", err)
	}
}

// TestUsageReplacesCumulativeReportsWithinACall is the regression for the bug this
// projection was built to fix. A provider that reports input and output in
// separate stream events sends two cumulative snapshots of the same call, and
// the old projection stored both and summed them, reporting the input twice.
func TestUsageReplacesCumulativeReportsWithinACall(t *testing.T) {
	repository := testRepository(t)
	start := testEvent(t, 1, &protocol.TurnStartedData{Provider: "fixture", Model: "model"})
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	// message_start reports input only; message_delta then reports the call's
	// running total including output.
	inputOnly := testEvent(t, 2, &protocol.UsageData{
		Sample: 1, Provider: "fixture", Model: "model",
		InputTokens: 100, CostMicrounits: 100, CostKnown: true,
	})
	withOutput := testEvent(t, 3, &protocol.UsageData{
		Sample: 1, Provider: "fixture", Model: "model",
		InputTokens: 100, OutputTokens: 50, CostMicrounits: 150, CostKnown: true,
	})
	for _, event := range []protocol.Event{inputOnly, withOutput} {
		if err := repository.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{TurnID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("aggregates = %+v", aggregates)
	}
	got := aggregates[0]
	if got.InputTokens != 100 || got.OutputTokens != 50 ||
		got.CostMicrounits != 150 || got.Calls != 1 {
		t.Fatalf("aggregate = %+v, want one call of 100 in / 50 out", got)
	}
	// An out-of-order replay of the earlier event must not undo the later one.
	if err := repository.Project(t.Context(), inputOnly); err != nil {
		t.Fatal(err)
	}
	aggregates, err = repository.QueryAggregates(t.Context(), Query{TurnID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := aggregates[0]; got.OutputTokens != 50 || got.CostMicrounits != 150 {
		t.Fatalf("stale replay changed the row: %+v", got)
	}
}

func TestUsageProjectionRefusesRegressionAndImpossibleDoubling(t *testing.T) {
	repository := testRepository(t)
	start := testEvent(t, 1, &protocol.TurnStartedData{Provider: "provider", Model: "model"})
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	baseline := testEvent(t, 2, &protocol.UsageData{
		Sample: 1, InputTokens: 100, OutputTokens: 20, ReasoningTokens: 4, CachedTokens: 10,
	})
	if err := repository.Project(t.Context(), baseline); err != nil {
		t.Fatal(err)
	}
	for _, event := range []protocol.Event{
		testEvent(t, 3, &protocol.UsageData{
			Sample: 1, InputTokens: 50, OutputTokens: 20, ReasoningTokens: 4, CachedTokens: 10,
		}),
		testEvent(t, 4, &protocol.UsageData{
			Sample: 1, InputTokens: 200, OutputTokens: 40, ReasoningTokens: 8, CachedTokens: 20,
		}),
	} {
		if err := repository.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{TurnID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 || aggregates[0].InputTokens != 100 ||
		aggregates[0].OutputTokens != 20 || aggregates[0].ReasoningTokens != 4 ||
		aggregates[0].CachedTokens != 10 {
		t.Fatalf("unsafe usage overwrite stored %+v", aggregates)
	}
}

// TestUsageSumsAcrossCallsAndSplitsUnpricedOnes covers the other half of the
// aggregation rule: separate provider calls do add up, and a zero cost is only
// reported as a cost when the model had pricing at all.
func TestUsageSumsAcrossCallsAndSplitsUnpricedOnes(t *testing.T) {
	repository := testRepository(t)
	start := testEvent(t, 1, &protocol.TurnStartedData{Provider: "provider", Model: "model"})
	if err := repository.Project(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	priced := testEvent(t, 2, &protocol.UsageData{
		Sample: 1, InputTokens: 10, OutputTokens: 5,
		CostMicrounits: 15, CostKnown: true,
	})
	unpriced := testEvent(t, 3, &protocol.UsageData{
		Sample: 2, InputTokens: 20, OutputTokens: 7,
	})
	for _, event := range []protocol.Event{priced, unpriced} {
		if err := repository.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{TurnID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("aggregates = %+v", aggregates)
	}
	got := aggregates[0]
	if got.InputTokens != 30 || got.OutputTokens != 12 || got.Calls != 2 {
		t.Fatalf("aggregate = %+v, want both calls counted", got)
	}
	if got.CostMicrounits != 15 || got.PricedCalls != 1 || got.UnpricedCalls != 1 {
		t.Fatalf("cost split = %+v, want 15 from one priced call and one unpriced", got)
	}
	rollup, err := repository.QueryRollup(
		t.Context(),
		Query{TurnID: "turn-1", Limit: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if rollup.Calls != 2 || rollup.InputTokens != 30 {
		t.Fatalf("paginated detail truncated rollup: %+v", rollup)
	}
}

func TestUsageRequiresTurnStartedContext(t *testing.T) {
	repository := testRepository(t)
	event := testEvent(t, 1, &protocol.UsageData{InputTokens: 1})
	if err := repository.Project(t.Context(), event); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("usage without context error = %v", err)
	}
	aggregates, err := repository.QueryAggregates(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 0 {
		t.Fatalf("failed transaction persisted usage: %+v", aggregates)
	}
}

func TestSessionUsageCanIncludeChildTurnsWithoutChangingDirectOwnership(
	t *testing.T,
) {
	repository := testRepository(t)
	addChildUsageTurn(t, repository)
	parentStart := testEvent(
		t,
		1,
		&protocol.TurnStartedData{Provider: "provider", Model: "model"},
	)
	childStart, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 2, OperationID: "operation-child",
		ThreadID: "thread-child", TurnID: "turn-child", ItemID: "item-child",
	}, &protocol.TurnStartedData{Provider: "provider", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	childUsage, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 4, OperationID: "operation-child",
		ThreadID: "thread-child", TurnID: "turn-child", ItemID: "item-child",
	}, &protocol.UsageData{Sample: 1, InputTokens: 20, OutputTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []protocol.Event{
		parentStart,
		testEvent(t, 3, &protocol.UsageData{
			Sample: 1, InputTokens: 10, OutputTokens: 1,
		}),
		childStart,
		childUsage,
	} {
		if err := repository.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := repository.QueryRollup(
		t.Context(),
		Query{SessionID: "session-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	inclusive, err := repository.QueryRollup(
		t.Context(),
		Query{SessionID: "session-1", IncludeChildren: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if direct.InputTokens != 10 || direct.Turns != 1 {
		t.Fatalf("direct rollup = %+v", direct)
	}
	if inclusive.InputTokens != 30 || inclusive.OutputTokens != 3 ||
		inclusive.Turns != 2 {
		t.Fatalf("inclusive rollup = %+v", inclusive)
	}
}

func addChildUsageTurn(t *testing.T, repository *Repository) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-child', 'workspace-1', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, status, created_at, updated_at)
		 VALUES ('thread-child', 'session-child', 'open', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-child', 'thread-child', 0, 'active', ?, ?)`,
	} {
		if _, err := repository.db.ExecContext(
			t.Context(),
			statement,
			now,
			now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.db.ExecContext(
		t.Context(),
		`INSERT INTO agent_nodes(
			workspace_root, session_id, agent_id, path, execution_root,
			thread_id, turn_id, status, revision, role,
			operation_id, actor, event_id, source_sequence, updated_at
		 ) VALUES (
			'/workspace', 'session-1', 'agent-1', '/root/agent-1', '/workspace',
			'thread-child', 'turn-child', 'running', 1, 'explore',
			'agent:1', 'test', 'event-agent-1', 1, ?
		 )`,
		now,
	); err != nil {
		t.Fatal(err)
	}
}

func testEvent(t *testing.T, sequence protocol.Cursor, data protocol.EventData) protocol.Event {
	t.Helper()
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: sequence, OperationID: "operation-1",
		ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1",
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func testModelMetadata(source string) *protocol.ModelMetadataProvenance {
	return &protocol.ModelMetadataProvenance{
		CanonicalID:  source,
		WireID:       source,
		Limits:       source,
		Capabilities: source,
		Pricing:      source,
	}
}

func testRepository(t *testing.T) *Repository {
	t.Helper()
	database, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-1', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-1', 'workspace-1', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, status, created_at, updated_at)
		 VALUES ('thread-1', 'session-1', 'open', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-1', 'thread-1', 0, 'active', ?, ?)`,
	} {
		if _, err := database.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return NewRepository(database.DB())
}
