package process

import (
	"errors"
	"os"
	"sort"
	"strings"
)

// allowedEnvironment is the host pass-through set: process-neutral
// variables every child may inherit implicitly. Language toolchain
// variables (GO*, cargo, python virtualenvs, ...) never pass through from
// the host: the environment preparer owns them on the sandboxed path, and
// model-declared env entries are explicit, reviewed input.
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

// SanitizedEnvironment filters host inheritance to the pass-through set and
// applies model-declared entries. Model entries are explicit input from the
// agent, which can already set any variable inside the command text, so the
// only hard rules are the NAME shape and the secret-name refusal; the
// declaration itself is journaled by the calling tool for audit.
func SanitizedEnvironment(extra []string) ([]string, error) {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !environmentAllowed(name) || SecretEnvironmentName(name) {
			continue
		}
		values[name] = value
	}
	for _, entry := range extra {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvironmentName(name) {
			return nil, errors.New("environment entries must use NAME=value")
		}
		if SecretEnvironmentName(name) {
			return nil, errors.New("secret environment variables cannot be passed to child processes")
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
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

func SecretEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{
		"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD",
		"CREDENTIAL", "AUTHORIZATION", "PRIVATE_KEY", "ACCESS_KEY", "COOKIE",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func environmentAllowed(name string) bool {
	return allowedEnvironment[name] || strings.HasPrefix(name, "LC_")
}

func extraEnvironmentNames(extra []string) map[string]bool {
	names := make(map[string]bool, len(extra))
	for _, entry := range extra {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name != "" {
			names[name] = true
		}
	}
	return names
}

func implicitLanguageEnvironmentName(name string) bool {
	switch name {
	case "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN",
		"GOFLAGS", "GO111MODULE", "GOENV", "GOPROXY", "GOPRIVATE", "GONOPROXY",
		"GOSUMDB", "GONOSUMDB", "GOVCS", "GOTMPDIR":
		return true
	default:
		return false
	}
}

func managedProxyEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	default:
		return false
	}
}

// dropHostLanguageEnvironment removes host-inherited language variables.
// Model extra and the prepared contract remain the only sources.
func dropHostLanguageEnvironment(environment, extra []string) []string {
	override := extraEnvironmentNames(extra)
	out := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if implicitLanguageEnvironmentName(name) && !override[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// applyPreparedEnvironment injects preparer-authored values that extra did
// not already set. Secret names and managed HTTP proxy variables stay out.
func applyPreparedEnvironment(environment, prepared, extra []string) []string {
	override := extraEnvironmentNames(extra)
	for _, entry := range prepared {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || override[name] {
			continue
		}
		if SecretEnvironmentName(name) || managedProxyEnvironmentName(name) {
			continue
		}
		environment = setEnvironmentValue(environment, name, value)
	}
	return environment
}
