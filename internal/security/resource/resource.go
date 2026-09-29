// Package resource is the single vocabulary for what an operation touches:
// resource kinds, access modes, and the loopback pseudo-resource.
package resource

import "strings"

const (
	KindFile      = "file"
	KindDirectory = "directory"
	KindRepo      = "repo"
	KindWorkspace = "workspace"
	KindHost      = "host"
	KindURL       = "url"
)

// IsPathKind reports whether kind names a filesystem location.
func IsPathKind(kind string) bool {
	switch kind {
	case KindFile, KindDirectory, KindRepo, KindWorkspace:
		return true
	}
	return false
}

// IsNetworkKind reports whether kind names a network endpoint.
func IsNetworkKind(kind string) bool {
	return kind == KindHost || kind == KindURL
}

type Access string

const (
	Read  Access = "read"
	Write Access = "write"
	Tree  Access = "tree"
	Use   Access = "use"
)

func (a Access) Valid() bool {
	switch a {
	case Read, Write, Tree, Use:
		return true
	}
	return false
}

// Writes reports whether a resource access mutates its target. On a resource,
// Tree is a tree-wide write. A tool descriptor's access mode has its own
// meaning (Tree there marks a whole-tree reader) and must not be passed here.
func (a Access) Writes() bool {
	return a == Write || a == Tree
}

// The loopback pseudo-resource grants a sandboxed process bind and connect on
// every local port. It is not an endpoint: it never matches a network grant
// and is always host-local.
const (
	LoopbackProtocol = "loopback"
	LoopbackHost     = "localhost"
	LoopbackTarget   = LoopbackProtocol + "://" + LoopbackHost + ":0"
)

// IsLoopback reports whether a resource is the loopback pseudo-resource.
func IsLoopback(kind, protocol string) bool {
	return kind == KindHost && protocol == LoopbackProtocol
}

// IsLoopbackTarget reports whether a canonical network target is the
// loopback pseudo-resource.
func IsLoopbackTarget(target string) bool {
	return strings.HasPrefix(
		strings.ToLower(strings.TrimSpace(target)), LoopbackProtocol+"://",
	)
}
