package wire

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestChildEnvironmentInheritsSnapshotAndRebindsPrivateCache(t *testing.T) {
	defaults := config.Defaults().Execution.Environment
	defaults.Resources = []config.EnvironmentResource{{
		Name: "test-cache", Namespace: "cache", Access: "write",
		Path: "sandbox-home/cache/fixture", Env: "FIXTURE_CACHE", Tree: true,
	}}
	parentHome := t.TempDir()
	options, _, err := bindEnvironmentSandbox(sandbox.Options{
		WorkspaceRoot: t.TempDir(), EnvironmentProfile: defaults.Profile,
	}, defaults, "", parentHome, []string{"LANG=C", "LC_MESSAGES=parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := newPlatformBackend(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(parent) })
	parentPolicy, _ := sandbox.BackendPolicy(parent)
	t.Setenv("LANG", "host-changed")
	t.Setenv("LC_MESSAGES", "host-changed")
	t.Setenv("SSL_CERT_FILE", "/missing-host-certificate")
	stateRoot, err := sandbox.CanonicalStateDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	toolsets := newChildToolsets(contentstore.NewMemory(contentstore.Options{}),
		webtool.Options{}, config.Verify{}, config.Journal{}, nil, nil, nil,
		"", 0, stateRoot, childSkillPaths(t, options.WorkspaceRoot))
	toolsets.environment = defaults
	toolsets.bindParentSandbox(parent)
	t.Cleanup(func() { _ = toolsets.closeAll(context.Background()) })
	seenCaches := make(map[string]bool)
	for range 2 {
		child, err := toolsets.open(t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		policy, ok := sandbox.BackendPolicy(child.backend)
		if !ok || policy.EnvironmentProfile != "isolated" || policy.SharedUserTemp {
			t.Fatal("child lost isolated posture")
		}
		if environmentEntryValue(policy.EnvironmentValues, "LANG") != "C" ||
			environmentEntryValue(policy.EnvironmentValues, "LC_MESSAGES") != "parent" ||
			environmentEntryValue(policy.EnvironmentValues, "PATH") != environmentEntryValue(parentPolicy.EnvironmentValues, "PATH") {
			t.Fatal("child recaptured host settings")
		}
		cache := environmentEntryValue(policy.EnvironmentValues, "FIXTURE_CACHE")
		if seenCaches[cache] {
			t.Fatal("siblings shared a private cache")
		}
		seenCaches[cache] = true
		if cache != filepath.Join(policy.PrivateTemp, "cache/fixture") || strings.HasPrefix(cache, parentHome) ||
			environmentEntryValue(policy.EnvironmentValues, "HOME") != policy.PrivateTemp {
			t.Fatalf("child binding: cache=%q HOME=%q privateTemp=%q parentHome=%q", cache, environmentEntryValue(policy.EnvironmentValues, "HOME"), policy.PrivateTemp, parentHome)
		}
	}
}
