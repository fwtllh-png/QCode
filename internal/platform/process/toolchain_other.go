//go:build !darwin

package process

func ensureGitToolchain(environment []string) []string {
	return environment
}
