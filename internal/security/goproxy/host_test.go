package goproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectHostUsesFirstProxyAndAllowsAllModules(t *testing.T) {
	binding, ok := InspectHost("https://user:secret@goproxy.example,direct")
	if !ok || binding.Upstream != "https://goproxy.example" ||
		binding.Credential.Kind != CredentialKindHost ||
		binding.Credential.Name != "goproxy.example" {
		t.Fatalf("binding = %+v ok=%v", binding, ok)
	}
	if strings.Contains(binding.Upstream, "secret") || strings.Contains(binding.Upstream, "user") {
		t.Fatalf("userinfo leaked into binding: %+v", binding)
	}
	if !MatchPrefix("code.byted.org/gopkg/ctxvalues", binding.Prefixes) ||
		!MatchPrefix("golang.org/x/sys", binding.Prefixes) {
		t.Fatalf("host GOPROXY must cover every module it already proxies: %v", binding.Prefixes)
	}
	if _, ok := InspectHost("off"); ok {
		t.Fatal("off GOPROXY must not bind")
	}
}

func TestLookupHostSecretUsesUserinfoThenNetrc(t *testing.T) {
	secret, ok := LookupHostSecret("https://user:token@goproxy.example|direct", "")
	if !ok || secret != "user:token" {
		t.Fatalf("userinfo secret = %q ok=%v", secret, ok)
	}
	netrc := filepath.Join(t.TempDir(), "netrc")
	if err := os.WriteFile(netrc, []byte("machine goproxy.example login bot password s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, ok = LookupHostSecret("https://goproxy.example,direct", netrc)
	if !ok || secret != "bot:s3cret" {
		t.Fatalf("netrc secret = %q ok=%v", secret, ok)
	}
	if _, ok := LookupHostSecret("https://goproxy.example", filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("missing netrc must not invent a credential")
	}
}

func TestParseNetrcIgnoresOtherMachines(t *testing.T) {
	secret, ok := parseNetrc(
		"machine other.example login a password b\nmachine goproxy.example login c password d\n",
		"goproxy.example",
	)
	if !ok || secret != "c:d" {
		t.Fatalf("netrc = %q ok=%v", secret, ok)
	}
	if _, ok := parseNetrc("machine other.example login a password b\n", "goproxy.example"); ok {
		t.Fatal("unrelated machine matched")
	}
}
