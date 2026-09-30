package skill

import "errors"

var (
	ErrDependencyConflict          = newRecoverableError("skill dependency conflict", ErrorCategoryDependencyConflict)
	ErrDependencyCycle             = newRecoverableError("skill dependency cycle", ErrorCategoryDependencyCycle)
	ErrCompatibilityMismatch       = newRecoverableError("skill compatibility mismatch", ErrorCategoryCompatibilityMismatch)
	ErrLockDrift                   = newRecoverableError("skill lock drift", ErrorCategoryLockDrift)
	ErrNotSelected                 = newRecoverableError("skill is not in this turn's catalog snapshot", ErrorCategoryNotSelected)
	ErrSkillHandleInvalid    error = &classifiedError{
		message: "skill handle is invalid or stale", category: ErrorCategoryHandleInvalid,
	}
	ErrSkillAmbiguous  = errors.New("skill name is ambiguous")
	ErrSelectionBudget = errors.New("skill selection budget exceeded")
)

const (
	ErrorCategoryDependencyConflict    = "dependency_conflict"
	ErrorCategoryDependencyCycle       = "dependency_cycle"
	ErrorCategoryCompatibilityMismatch = "compatibility_mismatch"
	ErrorCategoryLockDrift             = "skill_lock_drift"
	ErrorCategoryNotSelected           = "skill_not_selected"
	ErrorCategoryHandleInvalid         = "skill_handle_invalid"
)

type classifiedError struct {
	message  string
	category string
}

func (e *classifiedError) Error() string         { return e.message }
func (e *classifiedError) ErrorCategory() string { return e.category }

type recoverableError struct {
	classifiedError
}

func newRecoverableError(message, category string) error {
	return &recoverableError{classifiedError{message: message, category: category}}
}

// RecoverableCategory lets the result boundary recognize failures that can be
// returned to the model without importing Skill or authorizing an automatic retry.
func (e *recoverableError) RecoverableCategory() string { return e.category }

func ErrorCategory(err error) string {
	switch {
	case errors.Is(err, ErrDependencyConflict):
		return ErrorCategoryDependencyConflict
	case errors.Is(err, ErrDependencyCycle):
		return ErrorCategoryDependencyCycle
	case errors.Is(err, ErrCompatibilityMismatch):
		return ErrorCategoryCompatibilityMismatch
	case errors.Is(err, ErrLockDrift):
		return ErrorCategoryLockDrift
	case errors.Is(err, ErrNotSelected):
		return ErrorCategoryNotSelected
	case errors.Is(err, ErrSkillHandleInvalid):
		return ErrorCategoryHandleInvalid
	default:
		return ""
	}
}
