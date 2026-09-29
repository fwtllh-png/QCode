package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitysandbox "github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestPlanBaselineDriftRemainsAConflictBeforeStarting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := runtimeTestProfile()
	artifacts := &memoryArtifactStore{plan: protocol.SessionPlanArtifact{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "plan-drift", SessionID: "session-profile", ThreadID: "thread-profile",
		TurnID: "turn-plan", Cursor: 7, Status: protocol.PlanArtifactReady,
		Body:            `{"version":1,"revision":1,"steps":[{"id":"implement","title":"Update parser","status":"pending"}],"file_baseline":[{"path":"new.go","missing":true}]}`,
		ProfileRevision: profile.Revision, CanImplement: true, CanAutopilot: true,
		CreatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: &memoryProfileStore{profile: profile},
		DefaultProfile: profile, ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle: artifactLifecycle(), SessionArtifacts: artifacts,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	payload := &protocol.StartTurnPayload{
		ThreadID: "thread-profile", Prompt: "unchanged",
		PlanExecution: &protocol.PlanTransitionRequest{
			SessionID: "session-profile", PlanID: "plan-drift", Transition: protocol.PlanTransitionImplement,
		},
	}
	err := runtime.ArtifactService.PrepareStartPayload(t.Context(), root, payload)
	var drift *securitysandbox.PlanDriftError
	if protocol.CodeOf(err) != protocol.CodeConflict || !errors.As(err, &drift) || drift.Path != "new.go" {
		t.Fatalf("baseline drift lost conflict semantics: %v", err)
	}
	if payload.Prompt != "unchanged" {
		t.Fatal("rejected baseline changed the start payload")
	}
}
