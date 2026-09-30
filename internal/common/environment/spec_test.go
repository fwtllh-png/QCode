package environment

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestChildProfileIsAlwaysIsolated(t *testing.T) {
	if ChildProfile(ProfileNative) != ProfileIsolated ||
		ChildProfile(ProfileIsolated) != ProfileIsolated {
		t.Fatal("child profile must stay isolated")
	}
}

func TestStampDeclarationSetsSourceAndLifecycle(t *testing.T) {
	stamped := StampDeclaration(ResourceRequest{
		Name: "tool-cache", Namespace: NamespaceCache, Access: AccessWrite,
	})
	if stamped.Source != SourceUserDeclaration || stamped.Lifecycle != "workspace" {
		t.Fatalf("stamped cache = %+v", stamped)
	}
}

func TestValidateRequestRestrictsAccessUseToCredentials(t *testing.T) {
	if err := ValidateRequest(ResourceRequest{
		Name: "secret-file", Namespace: NamespaceHostConfig, Access: AccessUse,
		Source: "test",
	}); err == nil {
		t.Fatal("use on host_config must fail")
	}
	if err := ValidateRequest(ResourceRequest{
		Name: "proxy-credential", Namespace: NamespaceCredential, Access: AccessRead,
		Source: "test",
	}); err == nil {
		t.Fatal("credential read must fail")
	}
	if err := ValidateRequest(ResourceRequest{
		Name: "proxy-credential", Namespace: NamespaceCredential, Access: AccessUse,
		Host: "goproxy.example", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePostureRejectsSharedTempOnIsolated(t *testing.T) {
	if err := ValidatePosture(ProfileNative, true); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePosture(ProfileIsolated, true); err == nil {
		t.Fatal("isolated shared temp must fail")
	}
}

func TestEnvironmentContractHasNoImplementationDependencies(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			// Module imports have a domain in the first path component.
			if strings.Contains(strings.Split(name, "/")[0], ".") {
				t.Errorf("%s imports non-standard-library dependency %s", path, name)
			}
		}
	}
}
