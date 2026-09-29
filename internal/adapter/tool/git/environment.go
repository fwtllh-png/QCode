package git

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
)

// EnvironmentRequests describes Git's documented user configuration files.
// The caller supplies its captured source and decides whether the profile may
// inherit user configuration. Credential stores are never included.
func EnvironmentRequests(sourceEnv []string) []environment.ResourceRequest {
	var requests []environment.ResourceRequest
	for _, path := range gitUserConfigFiles(sourceEnv) {
		requests = append(requests, environment.ResourceRequest{
			Name:      "git-config:" + path,
			Namespace: environment.NamespaceHostConfig,
			Access:    environment.AccessRead,
			Path:      path,
			Source:    "git-config-locations",
			Lifecycle: "source_version",
			Purpose:   "host git configuration",
		})
	}
	return requests
}

func gitUserConfigFiles(sourceEnv []string) []string {
	var files []string
	home := envpolicy.Value(sourceEnv, "HOME")
	if path := existingRegularFile(strings.TrimSpace(envpolicy.Value(sourceEnv, "GIT_CONFIG_GLOBAL"))); path != "" {
		files = append(files, path)
	} else if filepath.IsAbs(home) {
		if path := existingRegularFile(filepath.Join(home, ".gitconfig")); path != "" {
			files = append(files, path)
		}
	}
	if xdg := strings.TrimSpace(envpolicy.Value(sourceEnv, "XDG_CONFIG_HOME")); filepath.IsAbs(xdg) {
		if path := existingRegularFile(filepath.Join(xdg, "git", "config")); path != "" {
			files = append(files, path)
		}
	} else if filepath.IsAbs(home) {
		if path := existingRegularFile(filepath.Join(home, ".config", "git", "config")); path != "" {
			files = append(files, path)
		}
	}
	return files
}

func existingRegularFile(path string) string {
	if path == "" || path == os.DevNull || !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}
