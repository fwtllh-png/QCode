package authority

import (
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
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
		WorkingDirectory: t.TempDir(), Effect: ManagedProcessEffect(securitymodel.RiskLow),
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
	if profile.Controls.Network != securitymodel.NetworkDenied || profile.Network.ProxyPort != 0 {
		t.Fatalf("network profile = %+v", profile.Network)
	}
}
