package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

const (
	ConstitutionFileName      = "constitution.json"
	ConstitutionSchemaVersion = 1
	DefaultConstitutionPrompt = "Follow the repository constitution. Mechanical holds cannot be bypassed."
)

// ConstitutionDocument is the on-disk constitution schema.
type ConstitutionDocument struct {
	Version        int      `json:"version"`
	DenyWriteGlobs []string `json:"deny_write_globs,omitempty"`
	HoldTools      []string `json:"hold_tools,omitempty"`
	DenyTools      []string `json:"deny_tools,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
}

// ConstitutionStatus is a doctor/setup-safe summary (no secrets).
type ConstitutionStatus struct {
	Loaded      bool     `json:"loaded"`
	UserPath    string   `json:"user_path,omitempty"`
	RepoPath    string   `json:"repo_path,omitempty"`
	UserPresent bool     `json:"user_present"`
	RepoPresent bool     `json:"repo_present"`
	RuleCount   int      `json:"rule_count"`
	PromptBytes int      `json:"prompt_bytes"`
	Sources     []string `json:"sources,omitempty"`
}

// ConstitutionBundle is the compiled constitution ready for policy + prompt injection.
type ConstitutionBundle struct {
	Rules   []Rule
	Prompt  string
	Status  ConstitutionStatus
	Sources []string
}

// LoadConstitution merges ~/.qcode/constitution.json and <workspace>/.qcode/constitution.json.
// Repository rules are prepended so they win on equal action priority.
func LoadConstitution(workspace, userHome string) (ConstitutionBundle, error) {
	if strings.TrimSpace(userHome) == "" {
		userHome = os.Getenv("HOME")
	}
	userPath := ""
	if userHome != "" {
		userPath = filepath.Join(userHome, pathpolicy.StateDir, ConstitutionFileName)
	}
	repoPath := filepath.Join(workspace, pathpolicy.StateDir, ConstitutionFileName)

	var userDoc, repoDoc ConstitutionDocument
	userPresent, err := readConstitution(userPath, &userDoc)
	if err != nil {
		return ConstitutionBundle{}, fmt.Errorf("user constitution: %w", err)
	}
	repoPresent, err := readConstitution(repoPath, &repoDoc)
	if err != nil {
		return ConstitutionBundle{}, fmt.Errorf("repo constitution: %w", err)
	}

	status := ConstitutionStatus{
		UserPath: userPath, RepoPath: repoPath,
		UserPresent: userPresent, RepoPresent: repoPresent,
	}
	if !userPresent && !repoPresent {
		return ConstitutionBundle{Status: status}, nil
	}

	userRules, err := compileConstitution(userDoc, "user")
	if err != nil {
		return ConstitutionBundle{}, fmt.Errorf("user constitution %s: %w", userPath, err)
	}
	repoRules, err := compileConstitution(repoDoc, "repo")
	if err != nil {
		return ConstitutionBundle{}, fmt.Errorf("repo constitution %s: %w", repoPath, err)
	}
	// Repo first so equal-priority ties prefer repository constitution.
	rules := append(append([]Rule{}, repoRules...), userRules...)

	prompt := strings.TrimSpace(repoDoc.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(userDoc.Prompt)
	}
	if prompt == "" && len(rules) > 0 {
		prompt = DefaultConstitutionPrompt
	}
	sources := make([]string, 0, 2)
	if repoPresent {
		sources = append(sources, repoPath)
	}
	if userPresent {
		sources = append(sources, userPath)
	}
	status.Loaded = true
	status.RuleCount = len(rules)
	status.PromptBytes = len(prompt)
	status.Sources = sources
	return ConstitutionBundle{Rules: rules, Prompt: prompt, Status: status, Sources: sources}, nil
}

// WriteConstitutionTemplate writes a minimal constitution.json if missing (or force).
func WriteConstitutionTemplate(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := ConstitutionDocument{
		Version:        ConstitutionSchemaVersion,
		DenyWriteGlobs: []string{"secrets/", "**/.env"},
		HoldTools:      []string{},
		DenyTools:      []string{},
		Prompt:         DefaultConstitutionPrompt,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func readConstitution(path string, doc *ConstitutionDocument) (bool, error) {
	if path == "" {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(doc); err != nil {
		return false, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("constitution contains multiple JSON values")
		}
		return false, err
	}
	if doc.Version != 0 && doc.Version != ConstitutionSchemaVersion {
		return false, fmt.Errorf("unsupported constitution version %d", doc.Version)
	}
	if doc.Version == 0 {
		doc.Version = ConstitutionSchemaVersion
	}
	return true, nil
}

func compileConstitution(doc ConstitutionDocument, source string) ([]Rule, error) {
	rules := make([]Rule, 0, len(doc.DenyWriteGlobs)+len(doc.HoldTools)+len(doc.DenyTools))
	for _, glob := range doc.DenyWriteGlobs {
		if strings.TrimSpace(glob) == "" {
			continue
		}
		resource, err := normalizeGlob(glob)
		if err != nil {
			return nil, fmt.Errorf("deny_write_globs entry %q: %w", glob, err)
		}
		// Write holds match every tool's write-access resources instead of a
		// hand-maintained tool list. A list cannot cover builtin writers added
		// later (file_apply, integrate_agent) nor external tools with trusted
		// write bindings; resource-scoped matching covers all of them while
		// leaving reads of the protected paths untouched.
		rules = append(rules, Rule{
			Tool: "*", Resource: resource, Action: ActionHold,
			Code: "constitution_hold:" + source, RequireWrite: true,
		})
	}
	for _, name := range doc.HoldTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		rules = append(rules, Rule{
			Tool: name, Resource: "*", Action: ActionHold,
			Code: "constitution_hold:" + source,
		})
	}
	for _, name := range doc.DenyTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		rules = append(rules, Rule{
			Tool: name, Resource: "*", Action: ActionDeny,
			Code: "constitution_deny:" + source,
		})
	}
	return rules, nil
}

// normalizeGlob accepts the policy path pattern syntax. A trailing "/", "/*",
// or "/**" protects the directory itself and its whole subtree. Unsupported
// syntax fails the load instead of leaving a path silently unprotected.
func normalizeGlob(glob string) (string, error) {
	glob = strings.TrimSpace(filepath.ToSlash(glob))
	glob = strings.TrimPrefix(glob, "./")
	glob = strings.TrimSuffix(glob, "/**")
	glob = strings.TrimSuffix(glob, "/*")
	glob = strings.TrimSuffix(glob, "/")
	glob = path.Clean(glob)
	if _, err := CompilePathPattern(glob); err != nil {
		return "", err
	}
	return glob, nil
}
