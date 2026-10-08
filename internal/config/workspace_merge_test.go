package config

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/workspacewrite"
)

func TestWorkspaceMergeBudgetConfiguration(t *testing.T) {
	for _, value := range []int{-1, 0, 1, workspacewrite.DefaultMergeDiffBytes + 1} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf("[execution]\nworkspace_merge_max_diff_bytes = %d\n", value))
			snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
			if value <= 0 {
				var fieldErr *FieldError
				if !errors.As(err, &fieldErr) || fieldErr.Field != fieldWorkspaceMergeMaxDiffBytes {
					t.Fatalf("invalid budget: %v", err)
				}
				return
			}
			if err != nil || snapshot.Config.Execution.WorkspaceMergeMaxDiffBytes != value {
				t.Fatalf("value=%d snapshot=%+v err=%v", value, snapshot, err)
			}
			if snapshot.Provenance[fieldWorkspaceMergeMaxDiffBytes] != SourceFile {
				t.Fatalf("provenance=%v", snapshot.Provenance[fieldWorkspaceMergeMaxDiffBytes])
			}
		})
	}
	snapshot, err := Load(LoadOptions{LookupEnv: envLookup(nil)})
	if err != nil || snapshot.Config.Execution.WorkspaceMergeMaxDiffBytes != workspacewrite.DefaultMergeDiffBytes || snapshot.Provenance[fieldWorkspaceMergeMaxDiffBytes] != SourceDefault {
		t.Fatalf("default budget: %+v %v", snapshot, err)
	}
	path := writeConfig(t, "[execution]\nworkspace_merge_max_diff_bytes = 1\n")
	snapshot, err = Load(LoadOptions{Path: path, LookupEnv: envLookup(map[string]string{"QCODE_WORKSPACE_MERGE_MAX_DIFF_BYTES": "2"})})
	if err != nil || snapshot.Config.Execution.WorkspaceMergeMaxDiffBytes != 2 || snapshot.Provenance[fieldWorkspaceMergeMaxDiffBytes] != SourceEnv {
		t.Fatalf("env budget: %+v %v", snapshot, err)
	}
}
