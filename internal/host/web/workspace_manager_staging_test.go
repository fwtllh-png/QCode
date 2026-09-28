package web

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/credential"
)

func TestRuntimeCredentialStagingStaysInTheDefaultNamespace(t *testing.T) {
	selection := webSetupSelection{
		Connections: []webSetupConnection{
			{ID: "openai", Provider: "openai"},
			{ID: "openai-compatible:2222", Provider: "openai-compatible:2222"},
		},
		DefaultConnection: "openai",
	}
	staged := credential.Reference{Kind: "keyring", Name: "staged-key"}
	control := &credential.Control{}
	tests := []struct {
		name        string
		stagedOwner string
		passes      bool
	}{
		{
			name: "reconfigure staging has no owner", stagedOwner: "",
			passes: true,
		},
		{
			name:        "editing the default connection through connection/add",
			stagedOwner: "openai", passes: true,
		},
		{
			name:        "second connection must not reach the default runtime",
			stagedOwner: "openai-compatible:2222", passes: false,
		},
		{
			name:        "unknown connection owner is not the default",
			stagedOwner: "missing", passes: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotControl, gotReference := runtimeCredentialStaging(
				selection, test.stagedOwner, control, staged,
			)
			if test.passes {
				if gotControl != control || gotReference != staged {
					t.Fatalf(
						"default staging was filtered: control=%v reference=%+v",
						gotControl != nil, gotReference,
					)
				}
				return
			}
			if gotControl != nil || gotReference != (credential.Reference{}) {
				t.Fatalf(
					"foreign staging reached the default runtime: control=%v reference=%+v",
					gotControl != nil, gotReference,
				)
			}
		})
	}

	// A nil control stays nil for every owner; nothing is invented.
	gotControl, gotReference := runtimeCredentialStaging(
		selection, "openai-compatible:2222", nil, credential.Reference{},
	)
	if gotControl != nil || gotReference != (credential.Reference{}) {
		t.Fatalf("nil staging changed: control=%v reference=%+v", gotControl, gotReference)
	}
}
