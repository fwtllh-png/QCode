// Package envprobe reports platform facts for the base system prompt without
// executing host tools. Tool versions are queried through ordinary tool calls
// when a task needs them.
package envprobe

import "runtime"

// Fingerprint returns facts about the running platform. The host login shell
// does not identify the shell used by a sandboxed command, so it is omitted.
func Fingerprint() []string {
	return []string{"os: " + runtime.GOOS + " (" + runtime.GOARCH + ")"}
}
