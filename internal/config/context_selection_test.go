package config

import (
	"fmt"
	"testing"
)

func TestCapacitySelectionZeroRetainsConfigurationProvenance(t *testing.T) {
	path := writeConfig(t, "[context.view]\nrecent_tail_turns = 0\n")
	for _, scenario := range []struct {
		name   string
		path   string
		env    map[string]string
		source Source
	}{
		{name: "file", path: path, source: SourceFile},
		{name: "environment", env: map[string]string{"QCODE_VIEW_RECENT_TAIL_TURNS": "0"}, source: SourceEnv},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot, err := Load(LoadOptions{Path: scenario.path, LookupEnv: envLookup(scenario.env)})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Config.Context.View.RecentTailTurns != 0 || snapshot.Provenance[fieldViewRecentTailTurns] != scenario.source {
				t.Fatalf("explicit zero was lost: view=%+v source=%v", snapshot.Config.Context.View, snapshot.Provenance[fieldViewRecentTailTurns])
			}
		})
	}
}

func TestCapacitySelectionTurnLimitBoundaries(t *testing.T) {
	for _, turns := range []int{-1, 0, 1, 128, 129} {
		t.Run(fmt.Sprint(turns), func(t *testing.T) {
			_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
				"QCODE_VIEW_RECENT_TAIL_TURNS": fmt.Sprint(turns),
			})})
			if valid := turns >= 0 && turns <= 128; (err == nil) != valid {
				t.Fatalf("turns=%d valid=%t error=%v", turns, valid, err)
			}
		})
	}
}
