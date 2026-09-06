// Command catalogcheck validates the committed bundled model catalog beyond
// the per-provider structural rules the runtime enforces: pricing sanity,
// limit relationships, provenance vocabulary, and cross-model consistency.
// The catalog is hand-maintained data; a wrong price or limit silently flows
// into usage accounting, so this gate keeps data rot out of releases.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(filepath.Join(*root, "internal", "adapter", "model", "catalog.v1.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("catalog check passed")
}

func run(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var document struct {
		Version   int              `json:"version"`
		Providers []model.Provider `json:"providers"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode catalog %s: %w", path, err)
	}
	if document.Version != 1 {
		return fmt.Errorf("catalog version %d is unsupported, want 1", document.Version)
	}
	// The production loading path enforces per-provider structure; reusing it
	// here keeps the checker exactly aligned with what the runtime accepts.
	if _, err := model.NewCatalog(document.Providers...); err != nil {
		return fmt.Errorf("runtime validation: %w", err)
	}

	var problems []string
	seenProviders := make(map[string]bool)
	for _, provider := range document.Providers {
		if seenProviders[provider.ID] {
			problems = append(problems, fmt.Sprintf("duplicate provider %q", provider.ID))
		}
		seenProviders[provider.ID] = true
		if problem := providerProvenanceProblem(provider.Provenance); problem != "" {
			problems = append(problems, fmt.Sprintf("provider %q: %s", provider.ID, problem))
		}
		canonicals := make(map[string]string)
		keys := make([]string, 0, len(provider.Models))
		for key := range provider.Models {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entry := provider.Models[key]
			label := fmt.Sprintf("provider %q model %q", provider.ID, key)
			if previous, exists := canonicals[entry.CanonicalID]; exists {
				problems = append(problems, fmt.Sprintf(
					"%s: canonical id %q already used by %q",
					label, entry.CanonicalID, previous,
				))
			}
			canonicals[entry.CanonicalID] = key
			if entry.Limits.MaxOutputTokens > entry.Limits.ContextTokens {
				problems = append(problems, fmt.Sprintf(
					"%s: max output %d exceeds context window %d",
					label, entry.Limits.MaxOutputTokens, entry.Limits.ContextTokens,
				))
			}
			if entry.Pricing.InputPerMillion < 0 || entry.Pricing.OutputPerMillion < 0 {
				problems = append(problems, fmt.Sprintf(
					"%s: negative pricing input=%v output=%v",
					label, entry.Pricing.InputPerMillion, entry.Pricing.OutputPerMillion,
				))
			}
			if entry.Pricing.Known && strings.TrimSpace(entry.Pricing.Currency) == "" {
				problems = append(problems, fmt.Sprintf(
					"%s: pricing is known but currency is empty", label,
				))
			}
			if problem := providerProvenanceProblem(model.Provenance(entry.Provenance)); problem != "" {
				problems = append(problems, fmt.Sprintf("%s: %s", label, problem))
			}
			if problem := effortProblem(entry.Capabilities); problem != "" {
				problems = append(problems, fmt.Sprintf("%s: %s", label, problem))
			}
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return fmt.Errorf("catalog check failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}

func providerProvenanceProblem(provenance model.Provenance) string {
	switch provenance {
	case "":
		return "provenance is required"
	case "bundled", "live", "manual":
		return ""
	default:
		return fmt.Sprintf("unknown provenance %q", string(provenance))
	}
}

func effortProblem(capabilities model.Capabilities) string {
	levels := capabilities.ReasoningEffortLevels()
	if len(levels) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		if strings.TrimSpace(level) == "" {
			return "reasoning effort level is empty"
		}
		if seen[level] {
			return fmt.Sprintf("duplicate reasoning effort level %q", level)
		}
		seen[level] = true
	}
	return ""
}
