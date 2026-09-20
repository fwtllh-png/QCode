package policy

import (
	"errors"
	"fmt"
)

type AuthoritySource string

const (
	SourceManaged    AuthoritySource = "managed"
	SourceUser       AuthoritySource = "user"
	SourceRepository AuthoritySource = "repository"
)

// UserRuleSource publishes validated, immutable workspace rules. Versions are
// nonzero and monotonic; an unchanged version returns no rules. Implementations
// must not perform I/O or call back into Runtime while serving a snapshot.
type UserRuleSource interface {
	UserRulesSince(version uint64) (rules []Rule, current uint64)
}

// BindUserRuleSource is a construction-time binding. CloneSampling freezes the
// current source version without transferring this live binding to the clone.
func (r *Runtime) BindUserRuleSource(source UserRuleSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userSource, r.userSourceVersion = source, 0
	r.refreshUserRulesLocked()
}

func (r *Runtime) refreshUserRulesLocked() {
	if r.userSource == nil {
		return
	}
	rules, version := r.userSource.UserRulesSince(r.userSourceVersion)
	if version <= r.userSourceVersion {
		return
	}
	r.User = rules
	r.userSourceVersion = version
	r.bumpRevisionLocked()
}

func (r *Runtime) ReloadSources(user, repository []Rule) (uint64, error) {
	if r == nil {
		return 0, errors.New("policy runtime is required")
	}
	if err := ValidateRules(SourceUser, user); err != nil {
		return 0, err
	}
	if err := ValidateRules(SourceRepository, repository); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userSource, r.userSourceVersion = nil, 0
	r.User = append([]Rule(nil), user...)
	r.Repository = append([]Rule(nil), repository...)
	return r.bumpRevisionLocked(), nil
}

func (r *Runtime) AppendUserRule(rule Rule) (uint64, error) {
	if r == nil {
		return 0, errors.New("policy runtime is required")
	}
	if err := ValidateRules(SourceUser, []Rule{rule}); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.User = append(append([]Rule(nil), r.User...), rule)
	return r.bumpRevisionLocked(), nil
}

func ValidateRules(source AuthoritySource, rules []Rule) error {
	switch source {
	case SourceManaged, SourceUser, SourceRepository:
	default:
		return errors.New("unknown policy authority source")
	}
	for index, rule := range rules {
		if rule.Tool == "" {
			return fmt.Errorf("%s rule %d: tool is required", source, index)
		}
		switch rule.Action {
		case ActionAllow, ActionAsk, ActionDeny, ActionHold:
		default:
			return fmt.Errorf("%s rule %d: action is invalid", source, index)
		}
		if source == SourceRepository && rule.Action == ActionAllow {
			return fmt.Errorf("repository rule %d: repository authority cannot allow", index)
		}
		if rule.Action == ActionHold && rule.Code == "" {
			return fmt.Errorf("%s rule %d: hold code is required", source, index)
		}
		if rule.CommandPrefix != "" {
			if _, err := parseStaticPrefix(rule.CommandPrefix); err != nil {
				return fmt.Errorf("%s rule %d: %w", source, index, err)
			}
			if rule.Action == ActionAllow && unsafePersistentPrefix(rule.CommandPrefix) {
				return fmt.Errorf(
					"%s rule %d: unsafe broad command prefix cannot persist",
					source, index,
				)
			}
		}
	}
	return nil
}
