package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/agentpreset"
	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type countingSessionReads struct {
	memorySessionLifecycleStore
	gets, threads, fences int
}

func (s *countingSessionReads) GetLifecycle(ctx context.Context, id string) (protocol.SessionSummary, error) {
	s.gets++
	return s.memorySessionLifecycleStore.GetLifecycle(ctx, id)
}

func (s *countingSessionReads) ThreadIDs(ctx context.Context, id string) ([]protocol.ThreadID, error) {
	s.threads++
	return s.memorySessionLifecycleStore.ThreadIDs(ctx, id)
}

func (s *countingSessionReads) PresentationReadFence(ctx context.Context, id string) (protocol.SessionReadFence, error) {
	s.fences++
	return s.memorySessionLifecycleStore.PresentationReadFence(ctx, id)
}

type countingProfileReads struct {
	memoryProfileStore
	ensures int
}

func (s *countingProfileReads) EnsureProfile(ctx context.Context, id string, defaults protocol.SessionProfile) (protocol.SessionProfile, error) {
	s.ensures++
	return s.memoryProfileStore.EnsureProfile(ctx, id, defaults)
}

type failingSidebarReads struct {
	memoryArtifactStore
	reads int
}

func (s *failingSidebarReads) CheckpointSummaries(context.Context, []string) (map[string]snapshot.CheckpointSummary, error) {
	s.reads++
	return nil, errors.New("sidebar checkpoint summaries are unavailable")
}

type countingWorkspaceRestore struct {
	memorySessionWorkspaces
	restores int
}

func (s *countingWorkspaceRestore) Restore(ctx context.Context, session string, thread protocol.ThreadID) (SessionWorkspace, error) {
	s.restores++
	return s.memorySessionWorkspaces.Restore(ctx, session, thread)
}

func TestSessionReadsAvoidSidebarAndReuseProfile(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		gets, threads, fences, profiles, restores int
		call                                      func(context.Context, *Runtime) error
	}{
		{"profile", 1, 0, 0, 1, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.SessionProfile(ctx, "session-profile")
			return err
		}},
		{"catalog", 1, 0, 0, 1, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.SessionToolCatalog(ctx, "session-profile")
			return err
		}},
		{"preset", 1, 0, 0, 1, 0, func(ctx context.Context, r *Runtime) error {
			profile := protocol.NewAgentPresetProfile(r.defaultProfile)
			profile.EnabledToolIDs = []string{"builtin:catalog_read"}
			_, err := r.AgentPresetService.Save(ctx, protocol.AgentPresetSaveRequest{
				SessionID: "session-profile", ID: "preset-read", Name: "Read", Profile: profile,
			})
			return err
		}},
		{"activate worktree", 1, 0, 0, 1, 1, func(ctx context.Context, r *Runtime) error {
			_, err := r.ActivateSession(ctx, ActivateSessionRequest{SessionID: "session-profile"})
			return err
		}},
		{"restore worktree profile", 1, 0, 0, 1, 1, func(ctx context.Context, r *Runtime) error {
			_, err := r.RestoreSessionProfile(ctx, "session-profile", "thread-profile")
			return err
		}},
		{"history", 1, 1, 0, 0, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.HistoryService.History(ctx, SessionHistoryQuery{SessionID: "session-profile", Limit: 10})
			return err
		}},
		{"snapshot", 0, 0, 1, 0, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.HistoryService.Snapshot(ctx, "session-profile")
			return err
		}},
		{"checkpoints", 1, 1, 0, 1, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.Checkpoints(ctx, "session-profile", 10)
			return err
		}},
		{"checkpoint", 1, 1, 0, 1, 0, func(ctx context.Context, r *Runtime) error {
			_, err := r.Checkpoint(ctx, "session-profile", "checkpoint-read")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, foreign := range []bool{false, true} {
				profile := runtimeTestProfile()
				profile.Provider, profile.Model = "test", "model"
				lifecycle := &countingSessionReads{memorySessionLifecycleStore: *artifactLifecycle()}
				lifecycle.summary.Isolation = SessionIsolationWorktree
				if foreign {
					lifecycle.summary.WorkspaceRoot = "/foreign"
				}
				profiles := &countingProfileReads{memoryProfileStore: memoryProfileStore{profile: profile}}
				artifacts := &failingSidebarReads{memoryArtifactStore: memoryArtifactStore{profile: profile, checkpoint: protocol.SessionCheckpoint{
					Version: protocol.CheckpointProtocolVersion, ID: "checkpoint-read", SessionID: "session-profile",
					ThreadID: "thread-profile", TurnID: "turn-read", Cursor: 1, Status: protocol.CheckpointCompleted,
					Summary: "Read checkpoint", ProfileRevision: profile.Revision, CreatedAt: time.Now().UTC(),
				}}}
				registry := tool.NewRegistry(nil, nil)
				if err := registry.Register(&profileCatalogTool{}); err != nil {
					t.Fatal(err)
				}
				workspaces := &countingWorkspaceRestore{}
				engine := NewThreadManager(func() (*EngineAdapter, error) {
					engine, err := newTestAgentEngine(agentengine.Options{
						ProviderConfig: agentengine.ProviderConfig{Provider: &threadEchoProvider{}, Route: runtimeTestRoute(t)},
						ToolConfig:     agentengine.ToolConfig{Tools: registry},
					})
					if err != nil {
						return nil, err
					}
					return AdaptEngine(engine), nil
				})
				runtime := NewRuntime(Options{
					WorkspaceRoot: "/workspace", Engine: engine, SessionLifecycle: lifecycle,
					SessionProfiles: profiles, DefaultProfile: profile, ProfileCapabilities: runtimeTestCapabilities(profile),
					SessionArtifacts: artifacts, AgentPresets: agentpreset.NewMemory(), ToolCatalog: registry, SessionWorkspaces: workspaces,
				})
				t.Cleanup(func() { closeRuntime(t, runtime) })
				err := tc.call(t.Context(), runtime)
				if foreign {
					if !protocol.IsCode(err, protocol.CodeConflict) || profiles.ensures != 0 || workspaces.restores != 0 || lifecycle.threads != 0 {
						t.Fatalf("foreign read: err=%v profiles=%d restores=%d threads=%d", err, profiles.ensures, workspaces.restores, lifecycle.threads)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if lifecycle.gets != tc.gets || lifecycle.threads != tc.threads || lifecycle.fences != tc.fences || profiles.ensures != tc.profiles || workspaces.restores != tc.restores {
					t.Fatalf("reads: lifecycle=%d threads=%d fences=%d profiles=%d restores=%d", lifecycle.gets, lifecycle.threads, lifecycle.fences, profiles.ensures, workspaces.restores)
				}
				if artifacts.reads != 0 {
					t.Fatalf("sidebar reads = %d", artifacts.reads)
				}
			}
		})
	}
}

func TestGitReadsSessionAndProfileOnce(t *testing.T) {
	runtime, root := newGitRuntime(t, true)
	gitFile(t, root, "a.txt", "after\n")
	gitFixture(t, root, "add", "a.txt")
	lifecycle := &countingSessionReads{memorySessionLifecycleStore: *artifactLifecycle()}
	lifecycle.summary.WorkspaceRoot = root
	profiles := &countingProfileReads{memoryProfileStore: memoryProfileStore{profile: runtimeTestProfile()}}
	artifacts := &failingSidebarReads{}
	runtime.sessionLifecycle, runtime.profiles, runtime.sessionArtifacts = lifecycle, profiles, artifacts
	runtime.defaultProfile = profiles.profile
	runtime.profileCapabilities = runtimeTestCapabilities(profiles.profile)
	request := gitRequest(t, runtime, "commit")
	request.SessionID = lifecycle.summary.SessionID
	result, err := runtime.ExecuteGit(t.Context(), request)
	if err != nil || result.Problem != nil {
		t.Fatalf("Git result=%+v error=%v", result, err)
	}
	if lifecycle.gets != 1 || lifecycle.threads != 1 || profiles.ensures != 1 || artifacts.reads != 0 {
		t.Fatalf("Git reads: lifecycle=%d threads=%d profiles=%d sidebar=%d", lifecycle.gets, lifecycle.threads, profiles.ensures, artifacts.reads)
	}
}

func TestCheckpointReadsKeepLivePreconditions(t *testing.T) {
	for _, state := range []string{"idle", "active fork", "accepted", "approval", "input", "withdrawn"} {
		t.Run(state, func(t *testing.T) {
			profile := runtimeTestProfile()
			lifecycle := artifactLifecycle()
			lifecycle.threadIDs = []protocol.ThreadID{"thread-profile", "thread-fork"}
			withdrawals := &appWithdrawalStore{}
			if state == "withdrawn" {
				lifecycle.summary.Status = protocol.SessionStatusRunning
				lifecycle.summary.LatestTurnID = "turn-withdrawn"
				withdrawals.withdrawn = true
			}
			artifacts := &failingSidebarReads{memoryArtifactStore: memoryArtifactStore{checkpoint: protocol.SessionCheckpoint{
				Version: protocol.CheckpointProtocolVersion, ID: "checkpoint-read", SessionID: "session-profile",
				ThreadID: "thread-profile", TurnID: "turn-read", Cursor: 1, Status: protocol.CheckpointCompleted,
				Summary: "Read checkpoint", ProfileRevision: profile.Revision, CreatedAt: time.Now().UTC(),
			}}}
			runtime := NewRuntime(Options{
				WorkspaceRoot: "/workspace", SessionLifecycle: lifecycle, SessionArtifacts: artifacts,
				SessionProfiles: &memoryProfileStore{profile: profile}, DefaultProfile: profile,
				ProfileCapabilities: runtimeTestCapabilities(profile), ContextRebaseStore: withdrawals,
			})
			t.Cleanup(func() { closeRuntime(t, runtime) })
			switch state {
			case "active fork":
				if _, err := runtime.active.Reserve("thread-fork", "turn-fork", "op-fork", ""); err != nil {
					t.Fatal(err)
				}
			case "accepted":
				runtime.OperationService.restore(map[protocol.OperationID]PendingOperation{"pending": {SessionID: "session-profile"}})
			case "approval":
				runtime.EventService.restore(RecoveryState{PendingApprovals: map[string]PendingApproval{"approval": {ThreadID: "thread-fork"}}})
			case "input":
				runtime.EventService.restore(RecoveryState{PendingInputs: map[string]PendingInput{"input": {ThreadID: "thread-fork"}}})
			}
			checkpoints, err := runtime.Checkpoints(t.Context(), "session-profile", 1)
			if err != nil {
				t.Fatal(err)
			}
			allowed := state == "idle" || state == "withdrawn"
			if len(checkpoints.Checkpoints) != 1 || checkpoints.Checkpoints[0].CanRestore != allowed || checkpoints.Checkpoints[0].CanFork != allowed {
				t.Fatalf("live preconditions lost: %+v", checkpoints)
			}
			if artifacts.reads != 0 {
				t.Fatalf("sidebar reads = %d", artifacts.reads)
			}
		})
	}
}

func TestPresetApplyKeepsRevisionCheckAfterProfileRead(t *testing.T) {
	profile := runtimeTestProfile()
	profiles := &countingProfileReads{memoryProfileStore: memoryProfileStore{profile: profile}}
	lifecycle := &countingSessionReads{memorySessionLifecycleStore: *artifactLifecycle()}
	presets := agentpreset.NewMemory()
	candidate := protocol.NewAgentPresetProfile(profile)
	candidate.ReasoningEffort = "high"
	if _, err := presets.Save(t.Context(), protocol.AgentPreset{ID: "preset-review", Name: "Review", Scope: protocol.AgentPresetScopeWorkspace, Profile: candidate}, 0); err != nil {
		t.Fatal(err)
	}
	engine := &profileTestEngine{beforeValidate: func() error {
		profiles.mu.Lock()
		profiles.profile.Revision++
		profiles.mu.Unlock()
		return nil
	}}
	runtime := NewRuntime(Options{WorkspaceRoot: "/workspace", Engine: engine, SessionLifecycle: lifecycle,
		SessionProfiles: profiles, AgentPresets: presets, DefaultProfile: profile, ProfileCapabilities: runtimeTestCapabilities(profile)})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	_, err := runtime.AgentPresetService.Apply(t.Context(), protocol.AgentPresetApplyRequest{
		SessionID: "session-profile", ThreadID: "thread-profile", PresetID: "preset-review", ExpectedProfileRevision: profile.Revision,
	})
	if err == nil || profiles.writes != 0 {
		t.Fatalf("stale preset error=%v writes=%d", err, profiles.writes)
	}
	if profiles.ensures != 1 || lifecycle.gets != 1 || lifecycle.threads != 0 {
		t.Fatalf("preset apply reads: profiles=%d lifecycle=%d threads=%d", profiles.ensures, lifecycle.gets, lifecycle.threads)
	}
}
