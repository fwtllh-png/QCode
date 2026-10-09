package envprep

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestCryptoConfigDefaultsAndExplicitDeclarations(t *testing.T) {
	config := filepath.Join(canonicalTestDir(t), "crypto.cnf")
	if err := os.WriteFile(config, []byte("# explicit crypto configuration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{environment.ProfileNative, environment.ProfileIsolated} {
		for _, tc := range []struct {
			name         string
			source       []string
			declarations []environment.ResourceRequest
			want         string
			readable     bool
		}{
			{name: "default", source: []string{}},
			{name: "explicit-empty", source: []string{"OPENSSL_CONF="}},
			{name: "source-does-not-grant-read", source: []string{"OPENSSL_CONF=" + config}, want: config},
			{
				name: "declared-file-overrides-source", source: []string{"OPENSSL_CONF=/unavailable-source.cnf"},
				declarations: []environment.ResourceRequest{{Name: "crypto", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Path: config, Env: "OPENSSL_CONF"}},
				want:         config, readable: true,
			},
			{
				name: "declared-empty-overrides-source", source: []string{"OPENSSL_CONF=" + config},
				declarations: []environment.ResourceRequest{{Name: "crypto", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Env: "OPENSSL_CONF", Value: ""}},
			},
		} {
			t.Run(profile+"/"+tc.name, func(t *testing.T) {
				prepared, err := Prepare(t.Context(), Options{
					Sandbox:   sandbox.Options{WorkspaceRoot: canonicalTestDir(t), PrivateTemp: canonicalTestDir(t), EnvironmentProfile: profile},
					SourceEnv: tc.source, Declarations: tc.declarations,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(prepared.Sandbox.EnvironmentValues, "OPENSSL_CONF="+tc.want) {
					t.Fatalf("prepared OPENSSL_CONF differs from %q", tc.want)
				}
				if slices.Contains(prepared.Sandbox.HostReadFiles, config) != tc.readable {
					t.Fatal("crypto variable and file authority were conflated")
				}
				policy, err := sandbox.BuildPolicy(prepared.Sandbox)
				if err != nil || !slices.Contains(policy.EnvironmentValues, "OPENSSL_CONF="+tc.want) {
					t.Fatalf("policy changed prepared crypto configuration: %v", err)
				}
			})
		}
	}
}
