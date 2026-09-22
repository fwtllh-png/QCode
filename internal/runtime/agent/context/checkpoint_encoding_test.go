package agentcontext

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestTurnCheckpointFindingsEncoding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings TurnFindings
		present  bool
	}{
		{name: "zero"},
		{name: "empty slices", findings: TurnFindings{Sites: []string{}, SourceTurns: []uint64{}}},
		{name: "conclusion", findings: TurnFindings{Conclusion: "fixed"}, present: true},
		{name: "whitespace", findings: TurnFindings{Conclusion: " "}, present: true},
		{name: "sites", findings: TurnFindings{Sites: []string{"main.go:3"}}, present: true},
		{name: "source turns", findings: TurnFindings{SourceTurns: []uint64{1}}, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := TurnCheckpoint{Turn: 1, Status: CheckpointCompleted, Text: "done", Findings: tc.findings}
			raw, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(`"findings"`)) != tc.present {
				t.Fatalf("unexpected findings field: %s", raw)
			}
			if !tc.present && string(raw) != `{"turn":1,"status":"completed","text":"done"}` {
				t.Fatalf("absent findings changed checkpoint bytes: %s", raw)
			}
			var restored TurnCheckpoint
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			again, err := json.Marshal(CloneTurnCheckpoints([]TurnCheckpoint{restored})[0])
			if err != nil || !bytes.Equal(raw, again) {
				t.Fatalf("checkpoint encoding changed after restore: %s, %v", again, err)
			}
		})
	}
}

func TestContextEnvelopeRestoresCheckpointWithoutFindings(t *testing.T) {
	store := &manifestMemoryStore{}
	snapshot := manifestSnapshot(t, 1, []provider.Message{
		turnMessage(provider.RoleUser, "inspect", 1),
		turnMessage(provider.RoleAssistant, "done", 1),
	})
	manifest, err := BuildContextManifest(t.Context(), store, "thread-1", "turn-1", snapshot, nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := []TurnCheckpoint{{Turn: 1, Status: CheckpointCompleted, Text: "done"}}
	// Recreate the original wire contract independently of the new field's tag.
	// Each layer retains its original digest, just as an already saved session does.
	snapshot.TurnCheckpoints = checkpoints
	snapshot.Digest = ""
	snapshot.Digest = digestBytes(checkpointJSONWithoutFindings(t, snapshot))
	manifest.TurnCheckpoints = checkpoints
	manifest.ContextDigest = snapshot.Digest
	manifest.Digest = ""
	manifest.Digest = digestBytes(checkpointJSONWithoutFindings(t, manifest))
	accounting := AccountingDelta{TurnID: "turn-1"}
	accounting.Seal()
	envelope := ContextEnvelope{Version: ContextEnvelopeVersion, Manifest: manifest, Accounting: accounting}
	envelope.Digest = digestBytes(checkpointJSONWithoutFindings(t, envelope))
	raw := checkpointJSONWithoutFindings(t, envelope)

	decoded, err := DecodeContextEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := LoadContextManifest(t.Context(), store, decoded.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Digest != snapshot.Digest || len(restored.History) != 2 ||
		len(restored.TurnCheckpoints) != 1 || restored.TurnCheckpoints[0].Text != "done" {
		t.Fatal("restored context differs from the saved checkpoint")
	}
	tampered := bytes.Replace(raw, []byte(`"text":"done"`), []byte(`"text":"changed"`), 1)
	if _, err := DecodeContextEnvelope(tampered); err == nil {
		t.Fatal("accepted a changed checkpoint with its original digest")
	}
}

func checkpointJSONWithoutFindings(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(raw, []byte(`,"findings":{}`), nil)
}
