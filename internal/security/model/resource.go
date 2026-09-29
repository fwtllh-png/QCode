package model

import (
	"slices"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

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
	LoopbackScope    = "all-local-ports"
	// LoopbackNetworkMode is the sandbox network mode whose only reachable
	// endpoint is the loopback pseudo-resource.
	LoopbackNetworkMode = LoopbackProtocol
)

// IsLoopback reports whether a resource is the loopback pseudo-resource.
func IsLoopback(kind, protocol string) bool {
	return kind == KindHost && protocol == LoopbackProtocol
}

// ResourceClass is the security category of a resolved resource. The Kind names above
// are how an adapter declares a resource; Class is what assessment and grants
// reason about.
type ResourceClass uint8

const (
	ClassPath ResourceClass = iota + 1
	ClassNetwork
	ClassLoopback
	ClassProcess
	ClassAgent
	ClassPlan
	ClassSession
	// ClassNamed is a logical object (memory entry, branch, remote, or an
	// extension-declared kind) identified by its adapter kind and ID.
	ClassNamed
)

func (c ResourceClass) String() string {
	switch c {
	case ClassPath:
		return "path"
	case ClassNetwork:
		return "network"
	case ClassLoopback:
		return LoopbackProtocol
	case ClassProcess:
		return "process"
	case ClassAgent:
		return "agent"
	case ClassPlan:
		return "plan"
	case ClassSession:
		return "session"
	case ClassNamed:
		return "named"
	}
	return "unknown"
}

// Resource is one resolved resource in security vocabulary.
type Resource struct {
	Class  ResourceClass
	Access Access
	// Path is the Guard-canonical location of a ClassPath resource.
	Path string
	Tree bool
	// Network is the endpoint of a ClassNetwork resource. It is nil when the
	// declared endpoint does not parse; ID then keeps the raw text and the
	// resource is treated as unconstrained egress.
	// URL retains the resolved request address for path-sensitive rules and approval display.
	URL          string
	Network      *netpolicy.Target
	Methods      []string
	AllowPrivate bool
	// ID identifies process, agent, plan, session, and named resources.
	ID string
	// Name is the adapter kind of a ClassNamed resource.
	Name string
}

func (r Resource) Writes() bool {
	return r.Access.Writes()
}

// Location is the resource's address within its class.
func (r Resource) Location() string {
	switch {
	case r.Class == ClassPath:
		return r.Path
	case r.Class == ClassLoopback:
		return LoopbackScope
	case r.Network != nil:
		return r.Network.Key()
	}
	return r.ID
}

func (r Resource) label() string {
	if r.Class == ClassNamed {
		return r.Name
	}
	return r.Class.String()
}

func (r Resource) methods() []string {
	methods := make([]string, 0, len(r.Methods))
	for _, method := range r.Methods {
		methods = append(methods, strings.ToUpper(method))
	}
	slices.Sort(methods)
	return methods
}

// String is the human-readable identity shown in grant summaries.
func (r Resource) String() string {
	parts := []string{r.label(), r.Location(), string(r.Access)}
	if r.Tree {
		parts = append(parts, "tree")
	}
	if len(r.Methods) != 0 {
		parts = append(parts, strings.Join(r.methods(), ","))
	}
	if r.AllowPrivate {
		parts = append(parts, "private")
	}
	return strings.Join(parts, ":")
}

// Key is the unambiguous identity used in fingerprints: every field is
// length-prefixed so no field value can impersonate another.
func (r Resource) Key() string {
	var b strings.Builder
	for _, field := range []string{
		r.label(), r.Location(), string(r.Access), strconv.FormatBool(r.Tree),
		strings.Join(r.methods(), ","), strconv.FormatBool(r.AllowPrivate),
	} {
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
	}
	return b.String()
}

// Clone copies every mutable part of a resolved resource.
func (r Resource) Clone() Resource {
	r.Methods = slices.Clone(r.Methods)
	if r.Network != nil {
		target := *r.Network
		r.Network = &target
	}
	return r
}

func CloneResources(resources []Resource) []Resource {
	result := make([]Resource, len(resources))
	for i, resource := range resources {
		result[i] = resource.Clone()
	}
	return result
}
