//go:build darwin

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

func extraPlatformPATHDirectories() []string {
	var directories []string
	for _, path := range append([]string{"/etc/paths"}, pathsDFiles()...) {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			directories = append(directories, line)
		}
	}
	for _, directory := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if info, err := os.Stat(directory); err == nil && info.IsDir() {
			directories = append(directories, directory)
		}
	}
	return directories
}

func pathsDFiles() []string {
	matches, err := filepath.Glob("/etc/paths.d/*")
	if err != nil {
		return nil
	}
	return matches
}
