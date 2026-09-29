// Package envpolicy defines environment selection and declaration rules without
// reading the host or executing processes.
package envpolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// allowedEnvironment is the source baseline for process settings. Variables
// outside this set require an explicit, validated declaration.
var allowedEnvironment = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"LANG": true, "LC_ALL": true, "TERM": true, "COLORTERM": true,
	"USER": true, "LOGNAME": true, "SHELL": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
	"OPENSSL_CONF":      true,
	"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_NOSYSTEM": true,
	"SYSTEMROOT": true, "COMSPEC": true, "PATHEXT": true, "WINDIR": true,
}

// Baseline selects non-sensitive process settings from an explicit source.
// It never captures the host environment; nil and empty both mean no source.
func Baseline(source []string) []string {
	var selected []string
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if ok && environmentAllowed(name) && validateEntry(entry) == nil {
			selected = append(selected, entry)
		}
	}
	return selected
}

// ValidateDeclaredEnvironment rejects secrets, interpreter injection and
// policy-owned paths in both trusted resource and per-command declarations.
func ValidateDeclaredEnvironment(entries []string) error {
	if err := ValidatePreparedEnvironment(entries); err != nil {
		return err
	}
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		if policyOwnedEnvironmentName(name) {
			return fmt.Errorf("%s is policy-owned: the sandbox decides it in every posture and a declaration cannot move it", name)
		}
	}
	return nil
}

// ValidatePreparedEnvironment permits policy-generated HOME and temporary
// paths, but applies the same secret and interpreter-injection rules.
func ValidatePreparedEnvironment(entries []string) error {
	for _, entry := range entries {
		if err := validateEntry(entry); err != nil {
			return err
		}
	}
	_, err := Merge(entries)
	return err
}

func validateEntry(entry string) error {
	name, _, ok := strings.Cut(entry, "=")
	if !ok || !validEnvironmentName(name) || strings.IndexByte(entry, 0) >= 0 {
		return errors.New("environment entries must use NAME=value without NUL")
	}
	if SecretEnvironmentName(name) {
		return errors.New("secret environment variables cannot be passed to child processes")
	}
	if interpreterPreloadEnvironmentName(name) {
		return fmt.Errorf("%s changes how the command text is interpreted and cannot be declared; set it inside the command if the task truly needs it", name)
	}
	return nil
}

// Merge applies layers in increasing precedence. Conflicting values within
// one layer are errors, even if a later layer would replace the value.
func Merge(layers ...[]string) ([]string, error) {
	values := make(map[string]string)
	for _, layer := range layers {
		current := make(map[string]string)
		for _, entry := range layer {
			name, value, ok := strings.Cut(entry, "=")
			if !ok || !validEnvironmentName(name) || strings.IndexByte(entry, 0) >= 0 {
				return nil, errors.New("environment entries must use NAME=value without NUL")
			}
			if previous, exists := current[name]; exists && previous != value {
				return nil, fmt.Errorf("conflicting environment declarations for %s", name)
			}
			current[name] = value
		}
		for name, value := range current {
			values[name] = value
		}
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result, nil
}

// Value reads an already captured environment without consulting the host.
func Value(entries []string, name string) string {
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

// WithoutManagedProxy removes host or declaration proxy settings. The active
// execution authority supplies the managed proxy at the process boundary.
func WithoutManagedProxy(entries []string) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		if !ManagedProxyEnvironmentName(name) {
			result = append(result, entry)
		}
	}
	return result
}

func ManagedProxyEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	default:
		return false
	}
}

// interpreterPreloadEnvironmentNames are refused as model declarations
// because they change how the reviewed command text is interpreted (injected
// libraries, startup scripts, interpreter flags) instead of carrying data.
// The command text can still export them when a task genuinely needs to.
var interpreterPreloadEnvironmentNames = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
	"BASH_ENV": true, "ENV": true,
	"NODE_OPTIONS": true, "PYTHONSTARTUP": true,
	"PERL5OPT": true, "RUBYOPT": true,
}

func interpreterPreloadEnvironmentName(name string) bool {
	return interpreterPreloadEnvironmentNames[name]
}

// policyOwnedEnvironmentNames are written only by the sandbox policy or the
// environment preparer: HOME follows the posture (private sandbox home or
// the host home) and TMPDIR/TMP/TEMP follow the temp posture (private temp
// or the resolved shared user temp). One rule in every posture.
var policyOwnedEnvironmentNames = map[string]bool{
	"HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
}

func policyOwnedEnvironmentName(name string) bool {
	return policyOwnedEnvironmentNames[name]
}

// validEnvironmentName accepts portable environment variable names: a
// letter or underscore, then letters, digits, underscores, or dots.
func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		char := name[index]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char == '_', char == '.' && index > 0:
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// secretMarkers are substring markers for well-known secret-bearing names.
// A bare _KEY suffix (OPENAI_KEY, SIGNING_KEY, GCP_KEY) is also treated as
// secret: none of the well-known markers appear in those names, yet handing
// them to a child process is handing over a credential.
var secretMarkers = []string{
	"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD",
	"CREDENTIAL", "AUTHORIZATION", "PRIVATE_KEY", "ACCESS_KEY", "COOKIE",
}

func SecretEnvironmentName(name string) bool {
	// Unicode normalization first: fullwidth lookalikes (ＡＰＩ＿ＫＥＹ)
	// must fold to their ASCII counterparts before matching.
	upper := strings.ToUpper(norm.NFKC.String(name))
	for _, marker := range secretMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return strings.HasSuffix(upper, "_KEY")
}

func environmentAllowed(name string) bool {
	return allowedEnvironment[name] || strings.HasPrefix(name, "LC_")
}
