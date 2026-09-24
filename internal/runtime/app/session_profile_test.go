package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestPlanningPolicyIsNotMutable(t *testing.T) {
	planning := "required"
	err := validateMutableProfilePatch(
		protocol.SessionProfilePatch{PlanningPolicy: &planning},
		[]string{"max_steps"},
	)
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("planning policy mutation error = %v", err)
	}
}

type memoryProfileStore struct {
	mu      sync.Mutex
	profile protocol.SessionProfile
	writes  int
}

func (s *memoryProfileStore) Profile(
	context.Context,
	string,
	protocol.SessionProfile,
) (protocol.SessionProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profile, nil
}

func (s *memoryProfileStore) EnsureProfile(
	ctx context.Context,
	sessionID string,
	defaults protocol.SessionProfile,
) (protocol.SessionProfile, error) {
	return s.Profile(ctx, sessionID, defaults)
}

func (s *memoryProfileStore) UpdateProfile(
	_ context.Context,
	_ string,
	expected uint64,
	_ protocol.SessionProfile,
	patch protocol.SessionProfilePatch,
) (protocol.SessionProfileUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != s.profile.Revision {
		return protocol.SessionProfileUpdateResult{}, errors.New("stale revision")
	}
	updated, err := protocol.ApplySessionProfilePatch(s.profile, patch)
	if err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	s.profile = updated.Profile
	s.writes++
	return updated, nil
}

type profileTestEngine struct {
	testEngine
	mu             sync.Mutex
	applied        protocol.SessionProfile
	beforeValidate func() error
}

type observingEventStore struct {
	EventStore
	onAppend func(protocol.Event)
}

func (s *observingEventStore) Append(
	ctx context.Context,
	event protocol.Event,
) error {
	if err := s.EventStore.Append(ctx, event); err != nil {
		return err
	}
	if s.onAppend != nil {
		s.onAppend(event)
	}
	return nil
}

func (e *profileTestEngine) ValidateSessionProfile(
	_ protocol.ThreadID,
	profile protocol.SessionProfile,
) error {
	if e.beforeValidate != nil {
		if err := e.beforeValidate(); err != nil {
			return err
		}
	}
	return profile.Validate()
}

func (e *profileTestEngine) ApplySessionProfile(
	_ protocol.ThreadID,
	profile protocol.SessionProfile,
) error {
	e.mu.Lock()
	e.applied = profile
	e.mu.Unlock()
	return nil
}

func TestSessionProfileUpdateRejectsActiveTurnBeforePersistence(t *testing.T) {
	defaults := runtimeTestProfile()
	store := &memoryProfileStore{profile: defaults}
	engine := &profileTestEngine{testEngine: testEngine{block: true}}
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     store,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-profile",
		TurnID:   "turn-profile",
		ItemID:   "item-profile",
		Prompt:   "wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if event := receiveEvent(t, events); event.Kind != protocol.EventTurnStarted {
		t.Fatalf("first event = %s", event.Kind)
	}
	effort := "high"
	_, err = runtime.UpdateSessionProfile(
		t.Context(),
		"session-profile",
		"thread-profile",
		1,
		protocol.SessionProfilePatch{ReasoningEffort: &effort},
	)
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("active update error = %v", err)
	}
	if store.writes != 0 {
		t.Fatalf("active update persisted %d writes", store.writes)
	}
}

func TestSessionProfileRejectsForeignWorkspace(t *testing.T) {
	defaults := runtimeTestProfile()
	lifecycle := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-foreign", ThreadID: "thread-foreign",
		Status: protocol.SessionStatusIdle, Isolation: "shared",
		WorkspaceRoot: "/workspace/foreign", WorkspaceLabel: "foreign",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		WorkspaceRoot:       "/workspace/current",
		SessionLifecycle:    lifecycle,
		SessionProfiles:     &memoryProfileStore{profile: defaults},
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	if _, err := runtime.SessionProfile(
		t.Context(),
		lifecycle.summary.SessionID,
	); err == nil {
		t.Fatal("foreign workspace profile was exposed")
	}
}

func TestRestoreSessionProfileRestoresIsolatedWorkspaceFirst(t *testing.T) {
	defaults := runtimeTestProfile()
	profiles := &memoryProfileStore{profile: defaults}
	workspaces := &memorySessionWorkspaces{}
	engine := &profileTestEngine{}
	engine.beforeValidate = func() error {
		if !workspaces.restored {
			return errors.New("profile validated before isolated workspace restore")
		}
		return nil
	}
	lifecycle := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-worktree-profile", ThreadID: "thread-worktree-profile",
		Title: "Worktree profile", Status: protocol.SessionStatusRunning,
		Isolation: SessionIsolationWorktree, WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace",
	}}
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     profiles,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
		SessionLifecycle:    lifecycle,
		SessionWorkspaces:   workspaces,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	if _, err := runtime.RestoreSessionProfile(
		t.Context(),
		lifecycle.summary.SessionID,
		lifecycle.summary.ThreadID,
	); err != nil {
		t.Fatal(err)
	}
	if !workspaces.restored {
		t.Fatal("isolated workspace was not restored")
	}
}

func TestRestoreChildSessionProfileDoesNotRestoreHostWorktree(t *testing.T) {
	defaults := runtimeTestProfile()
	workspaces := &memorySessionWorkspaces{}
	lifecycle := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-child-profile", ThreadID: "thread-host",
		Status: protocol.SessionStatusRunning, Isolation: SessionIsolationWorktree,
		WorkspaceRoot: "/workspace", WorkspaceLabel: "workspace",
	}}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: &memoryProfileStore{profile: defaults},
		DefaultProfile: defaults, ProfileCapabilities: runtimeTestCapabilities(defaults),
		SessionLifecycle: lifecycle, SessionWorkspaces: workspaces,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	if _, err := runtime.RestoreSessionProfile(
		t.Context(), lifecycle.summary.SessionID, "thread-child",
	); err != nil {
		t.Fatal(err)
	}
	if workspaces.restored {
		t.Fatal("child profile restore replaced the registered child engine")
	}
}

func TestSessionProfileRestoreIsIdempotentDuringRecoveredActiveTurn(
	t *testing.T,
) {
	defaults := runtimeTestProfile()
	store := &memoryProfileStore{profile: defaults}
	engine := &profileTestEngine{testEngine: testEngine{block: true}}
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     store,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if _, err := runtime.RestoreSessionProfile(
		t.Context(),
		"session-profile",
		"thread-profile",
	); err != nil {
		t.Fatal(err)
	}
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-profile",
		TurnID:   "turn-profile",
		ItemID:   "item-profile",
		Prompt:   "wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if event := receiveEvent(t, events); event.Kind != protocol.EventTurnStarted {
		t.Fatalf("first event = %s", event.Kind)
	}
	if _, err := runtime.RestoreSessionProfile(
		t.Context(),
		"session-profile",
		"thread-profile",
	); err != nil {
		t.Fatalf("same profile restore during active turn: %v", err)
	}

	store.mu.Lock()
	store.profile.Revision++
	store.mu.Unlock()
	if _, err := runtime.RestoreSessionProfile(
		t.Context(),
		"session-profile",
		"thread-profile",
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("changed profile restore during active turn error = %v", err)
	}
}

func TestTerminalEventIsPublishedAfterActiveTurnIsReleased(t *testing.T) {
	defaults := runtimeTestProfile()
	profiles := &memoryProfileStore{profile: defaults}
	observed := make(chan error, 1)
	store := &observingEventStore{EventStore: NewMemoryEventStore(16)}
	var runtime *Runtime
	store.onAppend = func(event protocol.Event) {
		if !protocol.IsTerminalEvent(event.Kind) {
			return
		}
		effort := "high"
		_, err := runtime.UpdateSessionProfile(
			context.Background(),
			"session-profile",
			event.ThreadID,
			defaults.Revision,
			protocol.SessionProfilePatch{ReasoningEffort: &effort},
		)
		observed <- err
	}
	runtime = NewRuntime(Options{
		Engine:              &profileTestEngine{},
		EventStore:          store,
		SessionProfiles:     profiles,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), startOperation(t, 1)); err != nil {
		t.Fatal(err)
	}
	for {
		event := receiveEvent(t, events)
		if protocol.IsTerminalEvent(event.Kind) {
			break
		}
	}
	if err := <-observed; err != nil {
		t.Fatalf("profile update at terminal publication = %v", err)
	}
}

func TestSessionProfileUpdateAppliesRevisionAndCacheReset(t *testing.T) {
	defaults := runtimeTestProfile()
	store := &memoryProfileStore{profile: defaults}
	engine := &profileTestEngine{}
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     store,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	effort := "high"
	updated, err := runtime.UpdateSessionProfile(
		t.Context(),
		"session-profile",
		"thread-profile",
		1,
		protocol.SessionProfilePatch{ReasoningEffort: &effort},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != 2 || !updated.PromptCacheReset {
		t.Fatalf("updated profile = %+v", updated)
	}
	engine.mu.Lock()
	applied := engine.applied
	engine.mu.Unlock()
	if applied.Revision != 2 || applied.ReasoningEffort != effort {
		t.Fatalf("applied profile = %+v", applied)
	}
}

func TestSessionToolCatalogProjectsProfileSelection(t *testing.T) {
	defaults := runtimeTestProfile()
	store := &memoryProfileStore{profile: defaults}
	registry := tool.NewRegistry(nil, nil)
	catalogTool := &profileCatalogTool{}
	if err := registry.Register(catalogTool); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: store,
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
		ToolCatalog:         registry,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	catalog, err := runtime.SessionToolCatalog(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tools) < 2 {
		t.Fatalf("catalog = %+v", catalog)
	}
	for _, entry := range catalog.Tools {
		if !entry.Enabled || !entry.Guarded {
			t.Fatalf("default catalog entry = %+v", entry)
		}
	}
	enabled := []string{"builtin:catalog_read"}
	if _, err := runtime.UpdateSessionProfile(
		t.Context(), "session-profile", "thread-profile", 1,
		protocol.SessionProfilePatch{EnabledToolIDs: &enabled},
	); err != nil {
		t.Fatal(err)
	}
	catalog, err = runtime.SessionToolCatalog(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range catalog.Tools {
		if entry.Enabled != (entry.ID == "builtin:catalog_read") {
			t.Fatalf("selected catalog entry = %+v", entry)
		}
	}
	generation := catalog.Generation
	catalogTool.unavailable = true
	catalog, err = runtime.SessionToolCatalog(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Generation <= generation {
		t.Fatalf("availability did not advance generation: %+v", catalog)
	}
	for _, entry := range catalog.Tools {
		if entry.ID == "builtin:catalog_read" &&
			(entry.Availability != "unavailable" ||
				entry.UnavailableReason != "fixture disconnected" ||
				!entry.Enabled) {
			t.Fatalf("unavailable catalog entry = %+v", entry)
		}
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("catalog_read")
	if !ok {
		t.Fatal("catalog_read is missing")
	}
	if _, err := registry.Revoke(
		entry.Source,
		"catalog_read",
		registry.Generation(),
	); err != nil {
		t.Fatal(err)
	}
	catalog, err = runtime.SessionToolCatalog(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	var revoked *protocol.SessionToolCatalogEntry
	for index := range catalog.Tools {
		if catalog.Tools[index].ID == "builtin:catalog_read" {
			revoked = &catalog.Tools[index]
			break
		}
	}
	if revoked == nil || revoked.State != "revoked" ||
		revoked.Availability != "unavailable" || !revoked.Enabled {
		t.Fatalf("revoked catalog entry = %+v", revoked)
	}
}

func TestProjectToolSourceCoversUnifiedFamilies(t *testing.T) {
	tests := map[string]struct {
		name, source, kind string
	}{
		"builtin":  {name: "file_read", source: "builtin:file_read", kind: "builtin"},
		"mcp":      {name: "issues", source: "mcp:github", kind: "mcp"},
		"external": {name: "external_demo_run", source: "external:lifecycle", kind: "external"},
		"skill":    {name: "skills_read", source: "builtin:skills_read", kind: "skill"},
		"dynamic":  {name: "host_echo", source: "dynamic:3", kind: "dynamic"},
	}
	for label, test := range tests {
		t.Run(label, func(t *testing.T) {
			kind, _ := projectToolSource(test.name, test.source)
			if kind != test.kind {
				t.Fatalf("kind = %q, want %q", kind, test.kind)
			}
		})
	}
}

type profileCatalogTool struct{ unavailable bool }

func (t *profileCatalogTool) Descriptor() tool.Descriptor {
	descriptor := tool.Descriptor{
		Name: "catalog_read", Description: "Read the catalog fixture",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
	}
	if t.unavailable {
		descriptor.Availability = tool.AvailabilityUnavailable
		descriptor.UnavailableReason = "fixture disconnected"
	}
	return descriptor
}

func (*profileCatalogTool) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func TestSessionProfileReturnsCapabilitiesForSelectedModel(t *testing.T) {
	defaults := runtimeTestProfile()
	selected := defaults
	selected.Model = "fixture-reasoner"
	capabilities := runtimeTestCapabilities(defaults)
	capabilities.MutableFields = append(
		capabilities.MutableFields,
		"model",
	)
	selectedCapabilities := capabilities.ModelCapabilities
	selectedCapabilities.DisplayName = "Fixture Reasoner"
	selectedCapabilities.DefaultReasoningEffort = "medium"
	selectedCapabilities.SelectionMode = "hot"
	runtime := NewRuntime(Options{
		Engine:              &profileTestEngine{},
		SessionProfiles:     &memoryProfileStore{profile: selected},
		DefaultProfile:      defaults,
		ProfileCapabilities: capabilities,
		ProfileModels: map[string]protocol.ModelCapabilities{
			"fixture\x00fixture-reasoner": selectedCapabilities,
		},
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	snapshot, err := runtime.SessionProfile(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Capabilities.Model != selected.Model ||
		snapshot.Capabilities.ModelCapabilities.DisplayName !=
			"Fixture Reasoner" ||
		snapshot.Capabilities.ModelCapabilities.SelectionMode != "hot" {
		t.Fatalf("selected capabilities = %+v", snapshot.Capabilities)
	}
}

func TestSessionProfileRejectsModelWithoutExplicitMetadata(t *testing.T) {
	defaults := runtimeTestProfile()
	selected := defaults
	selected.Model = "model-entered-by-user"
	capabilities := runtimeTestCapabilities(defaults)
	capabilities.MutableFields = append(capabilities.MutableFields, "model")
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: &memoryProfileStore{profile: selected},
		DefaultProfile: defaults, ProfileCapabilities: capabilities,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	snapshot, err := runtime.SessionProfile(t.Context(), "session")
	if err == nil {
		t.Fatalf("custom model capabilities = %+v, want rejection", snapshot.Capabilities)
	}
}

func runtimeTestProfile() protocol.SessionProfile {
	return protocol.SessionProfile{
		Version:             protocol.SessionProfileVersion,
		Revision:            1,
		Mode:                "act",
		Provider:            "fixture",
		Model:               "fixture-model",
		ApprovalPosture:     "suggest",
		ExecutionTarget:     "local",
		MaxSteps:            32,
		PromptCacheRevision: 1,
	}
}

func runtimeTestCapabilities(
	profile protocol.SessionProfile,
) protocol.SessionProfileCapabilities {
	return protocol.SessionProfileCapabilities{
		Provider: profile.Provider,
		Model:    profile.Model,
		ModelCapabilities: protocol.ModelCapabilities{
			DisplayName:       profile.Model,
			ContextWindow:     128_000,
			MaxOutputTokens:   8_192,
			Streaming:         true,
			Reasoning:         true,
			PromptCache:       true,
			ParallelToolCalls: "unknown",
			ReasoningEfforts:  []string{"low", "medium", "high"},
			MetadataProvenance: protocol.ModelMetadataProvenance{
				CanonicalID: "fixture", WireID: "fixture",
				Limits: "fixture", Capabilities: "fixture", Pricing: "fixture",
			},
			CredentialStatus: "unknown",
			Availability:     "available",
			SelectionMode:    "restart_required",
		},
		MutableFields: []string{
			"reasoning_effort",
			"enabled_tool_ids",
			"approval_posture",
			"max_steps",
		},
	}
}
