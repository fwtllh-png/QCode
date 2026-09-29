package model

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

func TestResourceIdentity(t *testing.T) {
	tests := []struct {
		resource Resource
		text     string
		location string
	}{
		{Resource{Class: ClassPath, Path: "src/a.go", Access: Write}, "path:src/a.go:write", "src/a.go"},
		{Resource{Class: ClassPath, Path: ".", Tree: true, Access: Read}, "path:.:read:tree", "."},
		{
			Resource{
				Class: ClassNetwork, Access: Write, Methods: []string{"post", "GET"}, AllowPrivate: true,
				Network: &netpolicy.Target{Scheme: "http", Host: "10.0.0.1", Port: 8080},
			},
			"network:http://10.0.0.1:8080:write:GET,POST:private", "http://10.0.0.1:8080",
		},
		{Resource{Class: ClassNetwork, ID: "not a target", Access: Read}, "network:not a target:read", "not a target"},
		{Resource{Class: ClassLoopback, Access: Write}, "loopback:" + LoopbackScope + ":write", LoopbackScope},
		{Resource{Class: ClassAgent, ID: "agent-1", Access: Write}, "agent:agent-1:write", "agent-1"},
		{Resource{Class: ClassNamed, Name: "memory", ID: "user", Access: Read}, "memory:user:read", "user"},
	}
	keys := map[string]bool{}
	for _, test := range tests {
		if got := test.resource.String(); got != test.text {
			t.Fatalf("String() = %q, want %q", got, test.text)
		}
		if got := test.resource.Location(); got != test.location {
			t.Fatalf("Location() = %q, want %q", got, test.location)
		}
		if keys[test.resource.Key()] {
			t.Fatalf("duplicate key for %s", test.text)
		}
		keys[test.resource.Key()] = true
	}
	spoof := Resource{Class: ClassPath, Path: "a.go\x00write", Access: Read}
	if spoof.Key() == (Resource{Class: ClassPath, Path: "a.go", Access: Write}).Key() {
		t.Fatal("path text can forge another resource key")
	}
	if !(Resource{Class: ClassPath, Access: Tree}).Writes() ||
		(Resource{Class: ClassPath, Access: Read}).Writes() {
		t.Fatal("resource write detection is wrong")
	}
}

func TestAccessWrites(t *testing.T) {
	for access, writes := range map[Access]bool{
		Read: false, Use: false, Write: true, Tree: true, "": false, "delete": false,
	} {
		if got := access.Writes(); got != writes {
			t.Fatalf("%q.Writes() = %v, want %v", access, got, writes)
		}
	}
	if Access("delete").Valid() || !Tree.Valid() {
		t.Fatal("access validity is wrong")
	}
}

func TestLoopbackPseudoResource(t *testing.T) {
	if LoopbackScope != "all-local-ports" {
		t.Fatalf("loopback target = %q", LoopbackScope)
	}
	if !IsLoopback(KindHost, LoopbackProtocol) || IsLoopback(KindURL, LoopbackProtocol) ||
		IsLoopback(KindHost, "https") {
		t.Fatal("loopback resource detection is wrong")
	}

}

func TestKinds(t *testing.T) {
	for _, kind := range []string{KindFile, KindDirectory, KindRepo, KindWorkspace} {
		if !IsPathKind(kind) || IsNetworkKind(kind) {
			t.Fatalf("%s is not a path kind", kind)
		}
	}
	for _, kind := range []string{KindHost, KindURL} {
		if IsPathKind(kind) || !IsNetworkKind(kind) {
			t.Fatalf("%s is not a network kind", kind)
		}
	}
}
