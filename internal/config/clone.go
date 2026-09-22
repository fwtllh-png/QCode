package config

import "maps"

// CloneSnapshot returns an independent runtime-safe copy of one resolved
// configuration and its provenance.
func CloneSnapshot(snapshot Snapshot) Snapshot {
	provenance := make(map[string]Source, len(snapshot.Provenance))
	maps.Copy(provenance, snapshot.Provenance)
	snapshot.Provenance = provenance
	snapshot.Config.Route.Slots = maps.Clone(snapshot.Config.Route.Slots)
	if snapshot.Config.Diagnostics.Commands != nil {
		commands := make(
			map[string]DiagnosticCommand,
			len(snapshot.Config.Diagnostics.Commands),
		)
		for extension, command := range snapshot.Config.Diagnostics.Commands {
			command.Args = append([]string(nil), command.Args...)
			commands[extension] = command
		}
		snapshot.Config.Diagnostics.Commands = commands
	}
	snapshot.Config.Execution.Environment.Resources = cloneEnvironmentResources(
		snapshot.Config.Execution.Environment.Resources,
	)
	snapshot.Config.Execution.Environment.AuthServices = cloneEnvironmentAuthServices(
		snapshot.Config.Execution.Environment.AuthServices,
	)
	return snapshot
}
