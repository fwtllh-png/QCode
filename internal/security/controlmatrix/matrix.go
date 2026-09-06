// Package controlmatrix defines comparable execution controls shared by tool
// bindings, sandbox probes, authority profiles, and brokers.
package controlmatrix

import (
	"fmt"
	"strings"
)

type FilesystemRead string
type FilesystemWrite string
type Network string
type ProcessTree string
type CrossProcess string
type Syscall string
type IPC string
type PathIdentity string
type ArtifactOrigin string
type DurableRecovery string

const (
	FilesystemReadUnrestricted  FilesystemRead = "unrestricted"
	FilesystemReadDeclaredRoots FilesystemRead = "declared_roots"
	FilesystemReadExactPaths    FilesystemRead = "exact_paths"

	FilesystemWriteUnrestricted FilesystemWrite = "unrestricted"
	FilesystemWriteWorkspace    FilesystemWrite = "workspace_tree"
	FilesystemWriteExactPaths   FilesystemWrite = "exact_paths"
	FilesystemWriteDenied       FilesystemWrite = "denied"

	NetworkDirect        Network = "direct"
	NetworkProxyTargets  Network = "proxy_targets"
	NetworkLoopbackExact Network = "loopback_exact"
	NetworkDenied        Network = "denied"

	ProcessTreeUnmanaged    ProcessTree = "unmanaged"
	ProcessTreeGroupKill    ProcessTree = "group_kill"
	ProcessTreeJobObject    ProcessTree = "job_object"
	ProcessTreePIDNamespace ProcessTree = "pid_namespace"

	CrossProcessUnrestricted CrossProcess = "unrestricted"
	CrossProcessRestricted   CrossProcess = "restricted"
	CrossProcessIsolated     CrossProcess = "isolated"

	SyscallUnrestricted  Syscall = "unrestricted"
	SyscallDenyDangerous Syscall = "deny_dangerous"
	SyscallAllowlist     Syscall = "allowlist"

	IPCUnrestricted     IPC = "unrestricted"
	IPCUnixOnly         IPC = "unix_only"
	IPCPrivateNamespace IPC = "private_namespace"

	PathIdentityLexical            PathIdentity = "lexical"
	PathIdentityCanonical          PathIdentity = "canonical"
	PathIdentityDescriptorRelative PathIdentity = "descriptor_relative"

	ArtifactOriginUnverifiedPath   ArtifactOrigin = "unverified_path"
	ArtifactOriginVerifiedManifest ArtifactOrigin = "verified_manifest"
	ArtifactOriginBrokerSnapshot   ArtifactOrigin = "broker_snapshot"

	DurableRecoveryMemoryOnly           DurableRecovery = "memory_only"
	DurableRecoveryExternalJournal      DurableRecovery = "external_journal"
	DurableRecoveryResumableTransaction DurableRecovery = "resumable_transaction"
)

type Matrix struct {
	FilesystemRead  FilesystemRead  `json:"filesystem_read,omitempty"`
	FilesystemWrite FilesystemWrite `json:"filesystem_write,omitempty"`
	Network         Network         `json:"network,omitempty"`
	ProcessTree     ProcessTree     `json:"process_tree,omitempty"`
	CrossProcess    CrossProcess    `json:"cross_process,omitempty"`
	Syscall         Syscall         `json:"syscall,omitempty"`
	IPC             IPC             `json:"ipc,omitempty"`
	PathIdentity    PathIdentity    `json:"path_identity,omitempty"`
	ArtifactOrigin  ArtifactOrigin  `json:"artifact_origin,omitempty"`
	DurableRecovery DurableRecovery `json:"durable_recovery,omitempty"`
}

type Requirements Matrix

func (r Requirements) Validate() error { return Matrix(r).validate(true) }
func (m Matrix) Validate() error       { return m.validate(false) }
func (r Requirements) IsZero() bool    { return r == (Requirements{}) }

func (m Matrix) StringMap() map[string]string {
	projected := make(map[string]string, len(dimensionSpecs))
	for _, spec := range dimensionSpecs {
		projected[spec.key] = spec.value(m)
	}
	return projected
}

func (m Matrix) Identity() string {
	parts := make([]string, 0, len(dimensionSpecs))
	for _, spec := range dimensionSpecs {
		parts = append(parts, spec.value(m))
	}
	return strings.Join(parts, "/")
}

func (m Matrix) validate(allowEmpty bool) error {
	if m == (Matrix{}) && allowEmpty {
		return nil
	}
	for _, spec := range dimensionSpecs {
		value := spec.value(m)
		if value == "" && allowEmpty {
			continue
		}
		if value != "" && spec.valid(value) {
			continue
		}
		return fmt.Errorf("%s control %q is invalid", spec.key, value)
	}
	return nil
}

func (r Requirements) SatisfiedBy(actual Matrix) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := actual.Validate(); err != nil {
		return err
	}
	for _, spec := range dimensionSpecs {
		required := spec.value(Matrix(r))
		if required == "" {
			continue
		}
		provided := spec.value(actual)
		if !spec.satisfies(required, provided) {
			return fmt.Errorf("%s control requires %q, backend provides %q",
				spec.label, required, provided)
		}
	}
	return nil
}

func satisfiesNetwork(required, actual Network) bool {
	switch required {
	case "":
		return true
	case NetworkDirect:
		return validNetwork(actual)
	case NetworkProxyTargets:
		return actual == NetworkProxyTargets || actual == NetworkDenied
	case NetworkLoopbackExact:
		return actual == NetworkLoopbackExact || actual == NetworkDenied
	default:
		return required == actual
	}
}

func CanEnforceNetwork(capability, desired Network) bool {
	switch capability {
	case NetworkDenied:
		return desired == NetworkDenied ||
			desired == NetworkLoopbackExact
	case NetworkLoopbackExact:
		return desired == NetworkLoopbackExact || desired == NetworkDenied
	case NetworkProxyTargets:
		return desired == NetworkProxyTargets || desired == NetworkDenied
	case NetworkDirect:
		return desired == NetworkDirect
	default:
		return false
	}
}

func CanEnforceFilesystemWrite(
	capability, desired FilesystemWrite,
) bool {
	switch capability {
	case FilesystemWriteDenied:
		return desired == FilesystemWriteDenied
	case FilesystemWriteExactPaths:
		return desired == FilesystemWriteExactPaths ||
			desired == FilesystemWriteDenied
	case FilesystemWriteWorkspace:
		return desired == FilesystemWriteWorkspace
	case FilesystemWriteUnrestricted:
		return desired == FilesystemWriteUnrestricted
	default:
		return false
	}
}

func satisfiesOrdered(required, actual string, order []string) bool {
	if required == "" {
		return true
	}
	requiredIndex, actualIndex := -1, -1
	for index, value := range order {
		if value == required {
			requiredIndex = index
		}
		if value == actual {
			actualIndex = index
		}
	}
	return requiredIndex >= 0 && actualIndex >= requiredIndex
}

func satisfiesProcessTree(required, actual ProcessTree) bool {
	switch required {
	case "":
		return true
	case ProcessTreeUnmanaged:
		return validProcessTree(actual)
	case ProcessTreeGroupKill:
		return actual == ProcessTreeGroupKill ||
			actual == ProcessTreeJobObject ||
			actual == ProcessTreePIDNamespace
	default:
		return required == actual
	}
}

func validNetwork(value Network) bool {
	return value == NetworkDirect || value == NetworkProxyTargets ||
		value == NetworkLoopbackExact || value == NetworkDenied
}
func validProcessTree(value ProcessTree) bool {
	return value == ProcessTreeUnmanaged || value == ProcessTreeGroupKill ||
		value == ProcessTreeJobObject || value == ProcessTreePIDNamespace
}
