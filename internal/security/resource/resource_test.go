package resource

import "testing"

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
	if LoopbackTarget != "loopback://localhost:0" {
		t.Fatalf("loopback target = %q", LoopbackTarget)
	}
	if !IsLoopback(KindHost, LoopbackProtocol) || IsLoopback(KindURL, LoopbackProtocol) ||
		IsLoopback(KindHost, "https") {
		t.Fatal("loopback resource detection is wrong")
	}
	if !IsLoopbackTarget(" LOOPBACK://localhost:0") || IsLoopbackTarget("https://localhost:443") {
		t.Fatal("loopback target detection is wrong")
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
