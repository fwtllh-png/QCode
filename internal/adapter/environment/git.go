package environment

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
)

// Git discovers the documented user git config files as exact host_config
// reads. It does not scan Home and does not expose credential stores.
type Git struct{}

func (Git) Name() string { return "git" }

func (Git) Discover(
	_ context.Context,
	_ platformenv.DiscoverInput,
) ([]envcontract.ResourceRequest, []envcontract.Fact, error) {
	var requests []envcontract.ResourceRequest
	for _, path := range gitUserConfigFiles() {
		requests = append(requests, envcontract.ResourceRequest{
			Name:      "git-config:" + filepath.Base(path),
			Namespace: envcontract.NamespaceHostConfig,
			Access:    envcontract.AccessRead,
			Path:      path,
			Source:    "git-config-locations",
			Lifecycle: "source_version",
			Purpose:   "host git configuration",
		})
	}
	return requests, nil, nil
}

func gitUserConfigFiles() []string {
	var files []string
	if path := existingRegularFile(strings.TrimSpace(os.Getenv("GIT_CONFIG_GLOBAL"))); path != "" {
		files = append(files, path)
	} else if home, err := os.UserHomeDir(); err == nil {
		if path := existingRegularFile(filepath.Join(home, ".gitconfig")); path != "" {
			files = append(files, path)
		}
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); filepath.IsAbs(xdg) {
		if path := existingRegularFile(filepath.Join(xdg, "git", "config")); path != "" {
			files = append(files, path)
		}
	} else if home, err := os.UserHomeDir(); err == nil {
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
