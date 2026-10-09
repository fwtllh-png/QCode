package model

// dimensionSpec declares one control dimension: its JSON key, the human label
// used in requirement errors, the getter from a Controls, the set of valid
// values, and how a required value is satisfied by an effective value.
//
// This table is the single place dimensions are enumerated. StringMap,
// Identity, Validate, and SatisfiedBy all derive from it, and the
// completeness test in control_dimensions_test.go fails until every Controls field has
// a spec row and every spec names a field — adding a dimension is one typed
// field, its constants, and exactly one row here.
type dimensionSpec struct {
	key       string
	label     string
	values    []string
	value     func(Controls) string
	satisfies func(required, actual string) bool
}

func (s dimensionSpec) valid(value string) bool {
	for _, candidate := range s.values {
		if candidate == value {
			return true
		}
	}
	return false
}

// orderedDimension builds a spec whose values form a total order from
// weakest to strongest: a requirement is satisfied when the effective value
// sits at or beyond the required position.
func orderedDimension(
	key, label string,
	values []string,
	value func(Controls) string,
) dimensionSpec {
	return dimensionSpec{
		key: key, label: label, values: values, value: value,
		satisfies: func(required, actual string) bool {
			return satisfiesOrdered(required, actual, values)
		},
	}
}

// Platform-specific filtering is not comparable to a portable isolation
// guarantee. It satisfies only itself and the unrestricted requirement.
func platformFilteredDimension(key, label string, values []string, value func(Controls) string) dimensionSpec {
	spec := orderedDimension(key, label, values, value)
	spec.values = append(append([]string(nil), values...), "platform_filtered")
	spec.satisfies = func(required, actual string) bool {
		if required == "platform_filtered" || actual == "platform_filtered" {
			return required == "unrestricted" || required == actual
		}
		return satisfiesOrdered(required, actual, values)
	}
	return spec
}

// dimensionSpecs lists the dimensions in Controls field order. Identity()
// joins them in this order, so inserting or reordering rows changes
// identities and must be an explicit decision.
var dimensionSpecs = []dimensionSpec{
	orderedDimension("filesystem_read", "filesystem read",
		[]string{
			string(FilesystemReadUnrestricted),
			string(FilesystemReadDeclaredRoots),
			string(FilesystemReadExactPaths),
		},
		func(m Controls) string { return string(m.FilesystemRead) }),
	orderedDimension("filesystem_write", "filesystem write",
		[]string{
			string(FilesystemWriteUnrestricted),
			string(FilesystemWriteWorkspace),
			string(FilesystemWriteExactPaths),
			string(FilesystemWriteDenied),
		},
		func(m Controls) string { return string(m.FilesystemWrite) }),
	{
		key:   "network",
		label: "network",
		values: []string{
			string(NetworkDirect),
			string(NetworkProxyTargets),
			string(NetworkLoopbackAny),
			string(NetworkDenied),
		},
		value: func(m Controls) string { return string(m.Network) },
		satisfies: func(required, actual string) bool {
			return satisfiesNetwork(Network(required), Network(actual))
		},
	},
	{
		key:   "process_tree",
		label: "process tree",
		values: []string{
			string(ProcessTreeUnmanaged),
			string(ProcessTreeGroupKill),
			string(ProcessTreeJobObject),
			string(ProcessTreePIDNamespace),
		},
		value: func(m Controls) string { return string(m.ProcessTree) },
		satisfies: func(required, actual string) bool {
			return satisfiesProcessTree(ProcessTree(required), ProcessTree(actual))
		},
	},
	orderedDimension("cross_process", "cross-process",
		[]string{
			string(CrossProcessUnrestricted),
			string(CrossProcessRestricted),
			string(CrossProcessIsolated),
		},
		func(m Controls) string { return string(m.CrossProcess) }),
	platformFilteredDimension("syscall", "syscall",
		[]string{
			string(SyscallUnrestricted),
			string(SyscallDenyDangerous),
			string(SyscallAllowlist),
		},
		func(m Controls) string { return string(m.Syscall) }),
	platformFilteredDimension("ipc", "IPC",
		[]string{
			string(IPCUnrestricted),
			string(IPCUnixOnly),
			string(IPCPrivateNamespace),
		},
		func(m Controls) string { return string(m.IPC) }),
	orderedDimension("path_identity", "path identity",
		[]string{
			string(PathIdentityLexical),
			string(PathIdentityCanonical),
			string(PathIdentityDescriptorRelative),
		},
		func(m Controls) string { return string(m.PathIdentity) }),
	orderedDimension("artifact_origin", "artifact origin",
		[]string{
			string(ArtifactOriginUnverifiedPath),
			string(ArtifactOriginVerifiedManifest),
			string(ArtifactOriginBrokerSnapshot),
		},
		func(m Controls) string { return string(m.ArtifactOrigin) }),
	orderedDimension("durable_recovery", "durable recovery",
		[]string{
			string(DurableRecoveryMemoryOnly),
			string(DurableRecoveryExternalJournal),
			string(DurableRecoveryResumableTransaction),
		},
		func(m Controls) string { return string(m.DurableRecovery) }),
}
