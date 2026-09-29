package authority

import (
	"path/filepath"
	"slices"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestAmendAddsOnePathWithoutRemovingDenies(t *testing.T) {
	input := fixtureCompileInput(t)
	base, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.SandboxPolicy.WorkspaceRoot, "result.txt")
	request, err := RequestFromDenial(base, sandbox.Denial{
		Operation: sandbox.DenialWrite, Resource: path,
		ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
	})
	if err != nil {
		t.Fatal(err)
	}
	amended, err := Amend(base, request, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(amended.Filesystem.WritePaths, path) ||
		!slices.Equal(
			amended.Filesystem.DeniedWriteRoots,
			base.Filesystem.DeniedWriteRoots,
		) ||
		amended.Digest == base.Digest {
		t.Fatalf("amended profile = %+v", amended)
	}
}

func TestAmendRejectsControlPlaneAndCrossProfileRequest(t *testing.T) {
	input := fixtureCompileInput(t)
	base, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	request := AdditionalPermissionRequest{
		BaseProfileDigest: base.Digest,
		Permission: AdditionalPermission{
			Kind: AdditionalPathWrite,
			Resource: filepath.Join(
				input.SandboxPolicy.WorkspaceRoot,
				".qcode",
				"permissions.toml",
			),
		},
	}
	if _, err := Amend(base, request, 2); err == nil {
		t.Fatal("control-plane amendment succeeded")
	}
	request.Permission.Resource = filepath.Join(
		input.SandboxPolicy.WorkspaceRoot,
		"result.txt",
	)
	request.BaseProfileDigest = "another-profile"
	if _, err := Amend(base, request, 2); err == nil {
		t.Fatal("cross-profile amendment succeeded")
	}
}

func TestNetworkAmendmentIsHostPortScoped(t *testing.T) {
	base, err := compileProfileForTest(fixtureCompileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromDenial(base, sandbox.Denial{
		Operation: sandbox.DenialNetwork, Resource: "api.example.com",
		Protocol: "https", Port: 443,
		ReasonCode: sandbox.ReasonNetworkNotAuthorized,
	})
	if err != nil {
		t.Fatal(err)
	}
	amended, err := Amend(base, request, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(
		amended.Network.Targets,
		"https://api.example.com:443",
	) || amended.Controls.Network != securitymodel.NetworkDenied {
		t.Fatalf("network authority = %+v", amended.Network)
	}
}

func TestNonAmendableDenialFailsClosed(t *testing.T) {
	base, err := compileProfileForTest(fixtureCompileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RequestFromDenial(base, sandbox.Denial{
		Operation:  sandbox.DenialWrite,
		Resource:   base.Filesystem.WorkspaceRoot,
		ReasonCode: sandbox.ReasonWorkspaceTreeDenied,
	})
	if err == nil {
		t.Fatal("workspace tree denial became amendable")
	}
}
