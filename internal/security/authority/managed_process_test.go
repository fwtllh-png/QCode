package authority

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/effect"
)

func TestManagedProcessProfileDropsProxyForDeniedNetwork(t *testing.T) {
	subject, err := NewManagedProcessSubject(
		SubjectHost,
		"fixture",
		TrustHost,
		1,
		"/bin/sh",
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := BuildManagedProcessOperation(ManagedProcessInput{
		ID: "fixture", Tool: "fixture",
		WorkspaceID:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkspaceGeneration: 1, Subject: subject, Executable: "/bin/sh",
		WorkingDirectory: t.TempDir(), Effect: ManagedProcessEffect(effect.RiskLow),
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := BuildManagedProcessProfile(ManagedProfileInput{
		Operation: operation, Revision: 1, WorkspaceRoot: t.TempDir(),
		AllowNetwork: false, ManagedProxyPort: 43128,
		Enforcement: "none", Backend: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Network.Mode != "denied" || profile.Network.ProxyPort != 0 {
		t.Fatalf("network profile = %+v", profile.Network)
	}
}
