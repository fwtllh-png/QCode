package authority

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestCompileDerivesRequiredControlsFromTypedResources(t *testing.T) {
	cases := []struct {
		name      string
		resources func(root string) []tool.Resource
		write     securitymodel.FilesystemWrite
		network   securitymodel.Network
	}{
		{
			name:      "read only process",
			resources: func(string) []tool.Resource { return nil },
			network:   securitymodel.NetworkDenied,
		},
		{
			name: "exact write path",
			resources: func(root string) []tool.Resource {
				return []tool.Resource{{
					Kind: "file", Path: filepath.Join(root, "out.txt"), Access: tool.AccessWrite,
				}}
			},
			write:   securitymodel.FilesystemWriteExactPaths,
			network: securitymodel.NetworkDenied,
		},
		{
			name: "network target",
			resources: func(string) []tool.Resource {
				return []tool.Resource{{
					Kind: "host", ID: "proxy.golang.org", Protocol: "https", Port: 443,
					Access: tool.AccessRead, Methods: []string{"CONNECT"},
				}}
			},
			network: securitymodel.NetworkProxyTargets,
		},
		{
			name: "loopback only",
			resources: func(string) []tool.Resource {
				return []tool.Resource{loopbackResource()}
			},
			network: securitymodel.NetworkLoopbackAny,
		},
		{
			name: "network target wins over loopback",
			resources: func(string) []tool.Resource {
				return []tool.Resource{loopbackResource(), {
					Kind: "url", ID: "https://example.com/", Access: tool.AccessRead,
				}}
			},
			network: securitymodel.NetworkProxyTargets,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureCompileInput(t)
			root := input.SandboxPolicy.WorkspaceRoot
			appendFixtureResources(&input.Prepared, tc.resources(root)...)
			compiled, err := Compile(resolveCompileFixture(input))
			if err != nil {
				t.Fatal(err)
			}
			if compiled.Required.Network != tc.network ||
				compiled.Required.FilesystemWrite != tc.write ||
				compiled.Operation.Required != compiled.Required {
				t.Fatalf("required = %+v operation = %+v", compiled.Required, compiled.Operation.Required)
			}
			if tc.write != "" &&
				compiled.Required.PathIdentity != securitymodel.PathIdentityDescriptorRelative {
				t.Fatalf("write without descriptor-relative identity: %+v", compiled.Required)
			}
		})
	}
}

func TestCompileProfileNetworkUsesSharedControlDerivation(t *testing.T) {
	cases := []struct {
		name      string
		resources []tool.Resource
		policy    func(*sandbox.Policy)
		reach     sandbox.NetworkReach
		proxyPort uint16
	}{
		{name: "no network", reach: sandbox.ReachNone},
		{
			name:      "loopback only",
			resources: []tool.Resource{loopbackResource()},
			policy:    func(p *sandbox.Policy) { p.ManagedProxyPort = 43128 },
			reach:     sandbox.ReachLoopback,
		},
		{
			name: "proxied target",
			resources: []tool.Resource{{
				Kind: "url", ID: "https://example.com/", Access: tool.AccessRead,
			}},
			policy:    func(p *sandbox.Policy) { p.ManagedProxyPort = 43128 },
			reach:     sandbox.ReachTargets,
			proxyPort: 43128,
		},
		{
			name: "target without delivery channel",
			resources: []tool.Resource{{
				Kind: "url", ID: "https://example.com/", Access: tool.AccessRead,
			}},
			reach: sandbox.ReachTargets,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureCompileInput(t)
			input.Capability.ManagedProxy = true
			if tc.policy != nil {
				tc.policy(&input.SandboxPolicy)
			}
			appendFixtureResources(&input.Prepared, tc.resources...)
			compiled, err := Compile(resolveCompileFixture(input))
			if err != nil {
				t.Fatal(err)
			}
			want := sandbox.NetworkControl(input.Capability, input.SandboxPolicy, tc.reach)
			if compiled.Profile.Controls.Network != want ||
				compiled.Profile.Network.ProxyPort != tc.proxyPort {
				t.Fatalf("profile network = %+v controls = %s, want %s",
					compiled.Profile.Network, compiled.Profile.Controls.Network, want)
			}
		})
	}
}

func TestCompileOperationUsesTypedResources(t *testing.T) {
	input := fixtureCompileInput(t)
	root := input.SandboxPolicy.WorkspaceRoot
	appendFixtureResources(&input.Prepared,
		tool.Resource{Kind: "file", Path: filepath.Join(root, "out.txt"), Access: tool.AccessWrite},
		tool.Resource{Kind: "parallel", ID: "go-build", Access: tool.AccessWrite},
		loopbackResource(),
	)
	compiled, err := Compile(resolveCompileFixture(input))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, resource := range compiled.Operation.Resources {
		kinds = append(kinds, resource.Kind)
	}
	slices.Sort(kinds)
	if strings.Join(kinds, ",") != "loopback,path,path" {
		t.Fatalf("operation resource kinds = %v", kinds)
	}
	if compiled.Operation.SchemaVersion != OperationSchemaVersion ||
		compiled.Profile.SchemaVersion != SchemaVersion ||
		compiled.Profile.Process.Enforcement != sandbox.EnforcementStrong {
		t.Fatalf("authority = %+v", compiled)
	}
}

func TestAuthorityBindAttachesEvidenceWithoutMutatingSource(t *testing.T) {
	input := fixtureCompileInput(t)
	root := input.SandboxPolicy.WorkspaceRoot
	appendFixtureResources(&input.Prepared, tool.Resource{
		Kind: "file", Path: filepath.Join(root, "out.txt"), Access: tool.AccessWrite,
	})
	compiled, err := Compile(resolveCompileFixture(input))
	if err != nil {
		t.Fatal(err)
	}
	artifact := &ArtifactIntent{ManifestDigest: strings.Repeat("b", 64), Generation: 1}
	mutation := strings.Repeat("c", 64)
	bound, err := compiled.Bind(Evidence{Artifact: artifact, FileMutationDigest: mutation})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Operation.Digest == compiled.Operation.Digest ||
		bound.Operation.Artifact == nil ||
		bound.Operation.File == nil || bound.Operation.File.MutationDigest != mutation ||
		bound.Required.ArtifactOrigin != securitymodel.ArtifactOriginBrokerSnapshot ||
		bound.Operation.Required != bound.Required {
		t.Fatalf("bound operation = %+v", bound.Operation)
	}
	if compiled.Operation.Artifact != nil || compiled.Required.ArtifactOrigin != "" {
		t.Fatalf("Bind mutated the compiled authority: %+v", compiled.Operation)
	}
	artifact.Generation = 9
	if bound.Operation.Artifact.Generation != 1 {
		t.Fatal("bound operation aliases the caller's artifact intent")
	}
}

func TestAuthorityWithProfileRebindsHostRoots(t *testing.T) {
	input := fixtureCompileInput(t)
	toolchain := t.TempDir()
	binary := filepath.Join(toolchain, "bin", "go")
	appendFixtureResources(&input.Prepared, tool.Resource{
		Kind: "file", Path: binary, Access: tool.AccessRead,
	})
	compiled, err := Compile(resolveCompileFixture(input))
	if err != nil {
		t.Fatal(err)
	}
	outside := compiled.Profile
	outside.Filesystem.ReadRoots = []string{input.SandboxPolicy.WorkspaceRoot}
	outside.Digest, _ = profileDigest(outside)
	if _, err := compiled.WithProfile(outside); err == nil {
		t.Fatal("profile without the host root still bound the host resource")
	}
	other := compiled.Profile
	other.Tool = "other"
	other.Digest, _ = profileDigest(other)
	if _, err := compiled.WithProfile(other); err == nil {
		t.Fatal("profile for another tool was accepted")
	}
	tampered := compiled.Profile
	tampered.Filesystem.WorkspaceBaseWrite = true
	if _, err := compiled.WithProfile(tampered); err == nil {
		t.Fatal("tampered profile was accepted")
	}
}

func loopbackResource() tool.Resource {
	return tool.Resource{
		Kind: securitymodel.KindHost, ID: securitymodel.LoopbackHost,
		Access: tool.AccessWrite, Protocol: securitymodel.LoopbackProtocol,
		Methods: []string{"BIND", "CONNECT"}, AllowPrivate: true,
	}
}
