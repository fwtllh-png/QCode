// Package constitution loads and compiles QCode constitution.json into policy rules.
package constitution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

const (
	FileName    = "constitution.json"
	SchemaVer   = 1
	EmptyPrompt = "Follow the repository constitution. Mechanical holds cannot be bypassed."
)

// Document is the on-disk constitution schema.
type Document struct {
	Version        int      `json:"version"`
	DenyWriteGlobs []string `json:"deny_write_globs,omitempty"`
	HoldTools      []string `json:"hold_tools,omitempty"`
	DenyTools      []string `json:"deny_tools,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
}

// Status is a doctor/setup-safe summary (no secrets).
type Status struct {
	Loaded      bool     `json:"loaded"`
	UserPath    string   `json:"user_path,omitempty"`
	RepoPath    string   `json:"repo_path,omitempty"`
	UserPresent bool     `json:"user_present"`
	RepoPresent bool     `json:"repo_present"`
	RuleCount   int      `json:"rule_count"`
	PromptBytes int      `json:"prompt_bytes"`
	Sources     []string `json:"sources,omitempty"`
}

// Bundle is the compiled constitution ready for policy + prompt injection.
type Bundle struct {
	Rules   []policy.Rule
	Prompt  string
	Status  Status
	Sources []string
}

// Load merges ~/.qcode/constitution.json and <workspace>/.qcode/constitution.json.
// Repository rules are prepended so they win on equal action priority.
func Load(workspace, userHome string) (Bundle, error) {
	if strings.TrimSpace(userHome) == "" {
		userHome = os.Getenv("HOME")
	}
	userPath := ""
	if userHome != "" {
		userPath = filepath.Join(userHome, ".qcode", FileName)
	}
	repoPath := filepath.Join(workspace, ".qcode", FileName)

	var userDoc, repoDoc Document
	userPresent, err := readOptional(userPath, &userDoc)
	if err != nil {
		return Bundle{}, fmt.Errorf("user constitution: %w", err)
	}
	repoPresent, err := readOptional(repoPath, &repoDoc)
	if err != nil {
		return Bundle{}, fmt.Errorf("repo constitution: %w", err)
	}

	status := Status{
		UserPath: userPath, RepoPath: repoPath,
		UserPresent: userPresent, RepoPresent: repoPresent,
	}
	if !userPresent && !repoPresent {
		return Bundle{Status: status}, nil
	}

	userRules := compile(userDoc, "user")
	repoRules := compile(repoDoc, "repo")
	// Repo first so equal-priority ties prefer repository constitution.
	rules := append(append([]policy.Rule{}, repoRules...), userRules...)

	prompt := strings.TrimSpace(repoDoc.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(userDoc.Prompt)
	}
	if prompt == "" && len(rules) > 0 {
		prompt = EmptyPrompt
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
	return Bundle{Rules: rules, Prompt: prompt, Status: status, Sources: sources}, nil
}

// WriteTemplate writes a minimal constitution.json if missing (or force).
func WriteTemplate(path string, force bool) error {
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
	doc := Document{
		Version:        SchemaVer,
		DenyWriteGlobs: []string{"secrets/", ".env"},
		HoldTools:      []string{},
		DenyTools:      []string{},
		Prompt:         EmptyPrompt,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func readOptional(path string, doc *Document) (bool, error) {
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
	if doc.Version != 0 && doc.Version != SchemaVer {
		return false, fmt.Errorf("unsupported constitution version %d", doc.Version)
	}
	if doc.Version == 0 {
		doc.Version = SchemaVer
	}
	return true, nil
}

func compile(doc Document, source string) []policy.Rule {
	rules := make([]policy.Rule, 0, len(doc.DenyWriteGlobs)+len(doc.HoldTools)+len(doc.DenyTools))
	for _, glob := range doc.DenyWriteGlobs {
		resource := normalizeGlob(glob)
		if resource == "" {
			continue
		}
		// Write holds match every tool's write-access resources instead of a
		// hand-maintained tool list. A list cannot cover builtin writers added
		// later (file_apply, integrate_agent) nor external tools with trusted
		// write bindings; resource-scoped matching covers all of them while
		// leaving reads of the protected paths untouched.
		rules = append(rules, policy.Rule{
			Tool: "*", Resource: resource, Action: policy.ActionHold,
			Code: "constitution_hold:" + source, RequireWrite: true,
		})
	}
	for _, name := range doc.HoldTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		rules = append(rules, policy.Rule{
			Tool: name, Resource: "*", Action: policy.ActionHold,
			Code: "constitution_hold:" + source,
		})
	}
	for _, name := range doc.DenyTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		rules = append(rules, policy.Rule{
			Tool: name, Resource: "*", Action: policy.ActionDeny,
			Code: "constitution_deny:" + source,
		})
	}
	return rules
}

func normalizeGlob(glob string) string {
	glob = strings.TrimSpace(filepath.ToSlash(glob))
	glob = strings.TrimPrefix(glob, "./")
	glob = strings.TrimSuffix(glob, "/**")
	glob = strings.TrimSuffix(glob, "/*")
	glob = strings.TrimSuffix(glob, "/")
	return filepath.ToSlash(filepath.Clean(glob))
}
