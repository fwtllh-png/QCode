package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

var requiredJourneys = []string{
	"context_continuity",
	"cross_file_edit",
	"test_selection",
	"crash_recovery",
	"approval",
	"budget",
	"host_replay",
	"web_capacity",
	"web_browser_durability",
	"web_long_session",
	"web_streaming_soak",
}

var validLanes = map[string]bool{
	"hermetic": true, "platform-capability": true,
	"integration": true, "release": true,
}

type benchmarkManifest struct {
	SchemaVersion int                `json:"schema_version"`
	Journeys      []benchmarkJourney `json:"journeys"`
}

type benchmarkJourney struct {
	ID           string   `json:"id"`
	Lane         string   `json:"lane"`
	Capabilities []string `json:"capabilities"`
	Evidence     []string `json:"evidence"`
	Command      string   `json:"command"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := check(*root); err != nil {
		fmt.Fprintln(os.Stderr, "benchmark v2:", err)
		os.Exit(1)
	}
	fmt.Printf(
		"benchmark v2 manifest valid: %d required user journeys\n",
		len(requiredJourneys),
	)
}

func check(root string) error {
	data, err := os.ReadFile(filepath.Join(
		root,
		"testdata",
		"contracts",
		"benchmark-v2.json",
	))
	if err != nil {
		return err
	}
	var manifest benchmarkManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.SchemaVersion != 2 {
		return fmt.Errorf("schema_version = %d, want 2", manifest.SchemaVersion)
	}
	seen := make(map[string]bool, len(manifest.Journeys))
	for _, journey := range manifest.Journeys {
		if journey.ID == "" || journey.Command == "" || len(journey.Evidence) == 0 {
			return fmt.Errorf("journey %q is incomplete", journey.ID)
		}
		if seen[journey.ID] {
			return fmt.Errorf("duplicate journey %q", journey.ID)
		}
		if !validLanes[journey.Lane] {
			return fmt.Errorf("journey %q has invalid lane %q", journey.ID, journey.Lane)
		}
		if journey.Lane == "platform-capability" && len(journey.Capabilities) == 0 {
			return fmt.Errorf("journey %q omits its platform capability", journey.ID)
		}
		for _, evidence := range journey.Evidence {
			info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(evidence)))
			if statErr != nil || info.IsDir() {
				return fmt.Errorf("journey %q evidence %q does not exist", journey.ID, evidence)
			}
		}
		seen[journey.ID] = true
	}
	for _, required := range requiredJourneys {
		if !seen[required] {
			return fmt.Errorf("required journey %q is missing", required)
		}
	}
	if len(seen) != len(requiredJourneys) {
		return errors.New("benchmark v2 must contain exactly the required journeys")
	}
	return nil
}
