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
	staged := &credentialRotation{
		control:   &credential.Control{},
		reference: credential.Reference{Kind: "keyring", Name: "staged-key"},
		phase:     credentialRotationStaged,
	}
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
			got := runtimeCredentialStaging(selection, test.stagedOwner, staged)
			if test.passes {
				if got != staged {
					t.Fatalf("default staging was filtered: %+v", got)
				}
				return
			}
			if got != nil {
				t.Fatalf("foreign staging reached the default runtime: %+v", got)
			}
		})
	}

	// A nil rotation stays nil for every owner; nothing is invented.
	if got := runtimeCredentialStaging(selection, "openai-compatible:2222", nil); got != nil {
		t.Fatalf("nil staging changed: %+v", got)
	}
}
