package engine

import (
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func (e *Engine) ValidateSessionProfile(profile protocol.SessionProfile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.validateSessionProfileLocked(profile)
}

func (e *Engine) validateSessionProfileLocked(
	profile protocol.SessionProfile,
) error {
	if e.runningScope() != nil {
		return errors.New("session profile cannot change while a turn is active")
	}
	routes, err := e.routesForProfileLocked(profile)
	if err != nil {
		return err
	}
	if err := validateReasoningEffort(routes, profile.ReasoningEffort); err != nil {
		return err
	}
	for _, id := range profile.EnabledToolIDs {
		if _, _, ok := tool.ParseCatalogToolID(id); !ok {
			return fmt.Errorf("session profile tool id %q is invalid", id)
		}
	}
	return nil
}

func (e *Engine) applySessionPolicyLocked(profile protocol.SessionProfile) {
	if e.options.Security == nil {
		return
	}
	permission := effectiveProfilePermission(e.profileReadOnly, policy.Permission(profile.ApprovalPosture))
	e.options.Security.SetPermissionWithinCeiling(permission, e.options.ProfilePermissionCeiling)
	e.options.Security.ConfigurePlanning(policy.PlanningAdaptive)
}

func (e *Engine) routesForProfileLocked(
	profile protocol.SessionProfile,
) (model.RouteSet, error) {
	current := e.options.Routes.Act()
	if profile.Provider == current.ProviderID() &&
		profile.Model == current.Model().ID {
		return e.options.Routes, nil
	}
	route, ok := e.options.SelectableRoutes[model.RouteKey(profile.Provider, profile.Model)]
	if !ok {
		return model.RouteSet{}, errors.New(
			"session profile model is unavailable; configure explicit metadata before selecting it",
		)
	}
	return e.options.Routes.WithAct(route)
}
