package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

const PermissionsFileName = "permissions.toml"

type PermissionEntry struct {
	Tool          string `toml:"tool"`
	Resource      string `toml:"resource,omitempty"`
	CommandPrefix string `toml:"command_prefix,omitempty"`
	GrantKey      string `toml:"grant_key,omitempty"`
	Code          string `toml:"code,omitempty"`
}
type PermissionsDocument struct {
	Deny  []PermissionEntry `toml:"deny,omitempty"`
	Ask   []PermissionEntry `toml:"ask,omitempty"`
	Allow []PermissionEntry `toml:"allow,omitempty"`
}
type PermissionsBundle struct {
	Path    string
	Present bool
	Rules   []Rule
	Doc     PermissionsDocument
}

func LoadPermissions(path string) (PermissionsBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PermissionsBundle{Path: path}, nil
		}
		return PermissionsBundle{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return PermissionsBundle{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PermissionsBundle{}, errors.New("permissions authority must be a regular file")
	}
	var doc PermissionsDocument
	if err := toml.Unmarshal(data, &doc); err != nil {
		return PermissionsBundle{}, fmt.Errorf("parse permissions.toml: %w", err)
	}
	rules, err := compilePermissions(doc)
	if err != nil {
		return PermissionsBundle{}, err
	}
	return PermissionsBundle{Path: path, Present: true, Rules: rules, Doc: doc}, nil
}
func (b PermissionsBundle) Summary() (deny, ask, allow int) {
	return len(b.Doc.Deny), len(b.Doc.Ask), len(b.Doc.Allow)
}
func AppendPermissionAllow(path string, rule Rule) (PermissionsBundle, error) {
	if rule.Tool == "" {
		return PermissionsBundle{}, errors.New("allow rule tool is required")
	}
	if len(rule.GrantKey) != 64 {
		return PermissionsBundle{}, errors.New("allow rule grant_key must be a SHA-256")
	}
	rule.Action = ActionAllow
	bundle, err := LoadPermissions(path)
	if err != nil {
		return PermissionsBundle{}, err
	}
	entry := PermissionEntry{
		Tool: rule.Tool, Resource: rule.Resource,
		CommandPrefix: rule.CommandPrefix, GrantKey: rule.GrantKey, Code: rule.Code,
	}
	for _, existing := range bundle.Doc.Allow {
		if entryEqual(existing, entry) {
			return bundle, nil
		}
	}
	bundle.Doc.Allow = append(bundle.Doc.Allow, entry)
	if err := writePermissionsDocument(bundle.Path, bundle.Doc); err != nil {
		return PermissionsBundle{}, err
	}
	rules, err := compilePermissions(bundle.Doc)
	if err != nil {
		return PermissionsBundle{}, err
	}
	bundle.Present = true
	bundle.Rules = rules
	return bundle, nil
}
func RuleFromInvocation(invocation Invocation) (Rule, error) {
	grant, ok := GrantForInvocation(invocation)
	if !ok {
		return Rule{}, errors.New("invocation has no persistable typed grant")
	}
	return Rule{
		Tool: invocation.Tool, GrantKey: grant.Key, Action: ActionAllow,
		Code: "permissions_always_allow",
	}, nil
}
func compilePermissions(doc PermissionsDocument) ([]Rule, error) {
	var rules []Rule
	add := func(entries []PermissionEntry, action Action) error {
		for _, entry := range entries {
			if action == ActionAllow && len(entry.GrantKey) != 64 {
				return errors.New("permissions allow entry requires a SHA-256 grant_key")
			}
			rules = append(rules, Rule{
				Tool: entry.Tool, Resource: entry.Resource,
				CommandPrefix: entry.CommandPrefix, GrantKey: entry.GrantKey,
				Action: action, Code: entry.Code,
			})
		}
		return nil
	}
	if err := add(doc.Deny, ActionDeny); err != nil {
		return nil, err
	}
	if err := add(doc.Ask, ActionAsk); err != nil {
		return nil, err
	}
	if err := add(doc.Allow, ActionAllow); err != nil {
		return nil, err
	}
	if err := ValidateRules(SourceUser, rules); err != nil {
		return nil, err
	}
	return rules, nil
}
func writePermissionsDocument(path string, doc PermissionsDocument) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("permissions authority must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := toml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".permissions-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
func entryEqual(a, b PermissionEntry) bool {
	return a.Tool == b.Tool && a.Resource == b.Resource &&
		a.CommandPrefix == b.CommandPrefix && a.GrantKey == b.GrantKey &&
		a.Code == b.Code
}
