package engine

import (
	"sort"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func changedPaths(entries []turnkernel.TurnDiffEntry) []string {
	unique := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Path == "" {
			continue
		}
		unique[entry.Path] = struct{}{}
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
