package result_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
)

func TestSkillErrorsPreserveRecoveryContract(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		message     string
		category    string
		recoverable bool
	}{
		{"dependency_conflict", skill.ErrDependencyConflict, "skill dependency conflict", skill.ErrorCategoryDependencyConflict, true},
		{"dependency_cycle", skill.ErrDependencyCycle, "skill dependency cycle", skill.ErrorCategoryDependencyCycle, true},
		{"compatibility", skill.ErrCompatibilityMismatch, "skill compatibility mismatch", skill.ErrorCategoryCompatibilityMismatch, true},
		{"lock_drift", skill.ErrLockDrift, "skill lock drift", skill.ErrorCategoryLockDrift, true},
		{"not_selected", skill.ErrNotSelected, "skill is not in this turn's catalog snapshot", skill.ErrorCategoryNotSelected, true},
		{"invalid_handle", skill.ErrSkillHandleInvalid, "skill handle is invalid or stale", skill.ErrorCategoryHandleInvalid, false},
		{"ambiguous", skill.ErrSkillAmbiguous, "skill name is ambiguous", "", false},
		{"selection_budget", skill.ErrSelectionBudget, "skill selection budget exceeded", "", false},
		{"unclassified", errors.New("skill lock drift"), "skill lock drift", "", false},
	} {
		for _, wrap := range []struct {
			name string
			wrap func(error) error
		}{
			{"direct", func(err error) error { return err }},
			{"wrapped", func(err error) error { return fmt.Errorf("load skill: %w", err) }},
			{"joined", func(err error) error { return errors.Join(errors.New("load failed"), err) }},
		} {
			t.Run(test.name+"/"+wrap.name, func(t *testing.T) {
				err := wrap.wrap(test.err)
				if !errors.Is(err, test.err) || test.err.Error() != test.message {
					t.Fatalf("skill error identity or message changed: %v", err)
				}
				if category := skill.ErrorCategory(err); category != test.category {
					t.Fatalf("skill category = %q, want %q", category, test.category)
				}
				if category := toolresult.FailureCategory(err); category != test.category {
					t.Fatalf("result category = %q, want %q", category, test.category)
				}
				content, recoverable := toolresult.RecoverableFailure(err)
				if recoverable != test.recoverable ||
					(recoverable && content != err.Error()) || (!recoverable && content != "") {
					t.Fatalf("recovery = %q, %t; want recoverable=%t", content, recoverable, test.recoverable)
				}
				result, recovered := toolresult.RecoverResult(
					tool.NewRegistry(nil, nil), provider.ToolCall{Name: "unregistered"},
					tool.Result{Metadata: map[string]any{"skill": "fixture"}}, err,
				)
				if recovered != test.recoverable {
					t.Fatalf("recovered = %t, want %t", recovered, test.recoverable)
				}
				if !recovered {
					return
				}
				if !result.IsError || result.Content != err.Error() ||
					result.Metadata["error_category"] != test.category ||
					result.Metadata["skill"] != "fixture" || len(result.Metadata) != 2 {
					t.Fatalf("recovered result = %+v", result)
				}
				if result.Outcome == nil || result.Outcome.Facts.Failure == nil ||
					result.Outcome.Facts.Failure.Category != test.category {
					t.Fatalf("failure outcome = %+v", result.Outcome)
				}
			})
		}
	}
}

func TestSkillHandleRecoveryRetainsListAction(t *testing.T) {
	err := tool.WithRecoveryHint(fmt.Errorf("read skill: %w", skill.ErrSkillHandleInvalid), tool.RecoveryHint{
		ErrorCategory: skill.ErrorCategoryHandleInvalid, RequiredAction: "skills_list",
	})
	result, recovered := toolresult.RecoverResult(
		tool.NewRegistry(nil, nil), provider.ToolCall{Name: "unregistered"}, tool.Result{}, err,
	)
	if !recovered || !result.IsError || !errors.Is(err, skill.ErrSkillHandleInvalid) {
		t.Fatalf("handle recovery = %+v, %t", result, recovered)
	}
	if result.Metadata["error_category"] != skill.ErrorCategoryHandleInvalid ||
		result.Metadata["required_action"] != "skills_list" ||
		result.Metadata["retry_original"] != false ||
		!strings.Contains(result.Content, "required_action=skills_list; retry_original=false") {
		t.Fatalf("handle recovery guidance = %+v", result)
	}
}
