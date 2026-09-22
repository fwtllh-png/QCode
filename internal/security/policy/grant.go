package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type Grant struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Summary string `json:"summary"`
	// Prefix is the static argv of a single-segment shell command whose
	// reusable approval also matches later commands that extend the argv
	// within the same scope. It is nil for composite, dynamic, or
	// interpreter-payload commands, which keep exact-identity grants.
	Prefix []string `json:"prefix,omitempty"`

	// scope fingerprints everything a shell grant binds besides the
	// command (cwd and normalized resources) so prefix matching cannot
	// cross scope boundaries. It never serializes.
	scope string
}

func GrantForInvocation(call Invocation) (Grant, bool) {
	resources, kinds := normalizedGrantResources(call.Resources, false)
	kind, summary := "", ""
	hash := sha256.New()
	writeFingerprintField(hash, call.Tool)
	var prefix []string
	var cwd string
	switch {
	case call.Capability == tool.CapabilityProcess:
		var input struct {
			Command, CWD, Path string
			Args               []string
		}
		if json.Unmarshal(call.Arguments, &input) != nil {
			return Grant{}, false
		}
		input.Command = strings.TrimSpace(input.Command)
		if input.Command == "" && input.Path != "" {
			encoded, err := json.Marshal(struct {
				Path string   `json:"path"`
				Args []string `json:"args,omitempty"`
			}{
				Path: cleanGrantPath(input.Path),
				Args: append([]string(nil), input.Args...),
			})
			if err != nil {
				return Grant{}, false
			}
			input.Command = string(encoded)
		}
		if input.Command == "" && kinds&4 == 0 {
			return Grant{}, false
		}
		commandIdentity := input.Command
		if input.Command != "" {
			var ok bool
			commandIdentity, ok = commandGrantIdentity(input.Command)
			if !ok {
				return Grant{}, false
			}
			prefix = commandGrantPrefix(input.Command)
		}
		kind, summary = "shell", "command: "+input.Command
		if input.Command == "" {
			kind, summary = "sandbox", "sandbox escalation: "+strings.Join(resources, ", ")
		}
		writeFingerprintField(hash, commandIdentity)
		cwd = cleanGrantPath(input.CWD)
		writeFingerprintField(hash, cwd)
	case call.Journaled && len(resources) != 0:
		kind, summary = "file", "workspace paths: "+strings.Join(resources, ", ")
	case call.Capability == tool.CapabilityNetwork:
		resources, _ = normalizedGrantResources(call.Resources, true)
		if len(resources) == 0 {
			return Grant{}, false
		}
		kind, summary = "network", "network endpoints: "+strings.Join(resources, ", ")
	case kinds&2 != 0 && len(resources) != 0:
		kind, summary = "agent", call.Tool+": "+strings.Join(resources, ", ")
	default:
		return Grant{}, false
	}
	for _, resource := range resources {
		writeFingerprintField(hash, resource)
	}
	return Grant{
		Kind: kind, Key: hex.EncodeToString(hash.Sum(nil)),
		Summary: summary, Prefix: prefix,
		scope: grantScopeFingerprint(cwd, resources),
	}, true
}

// grantScopeFingerprint binds a shell grant prefix to its cwd and resource
// set: an approved prefix may only match later commands in the same scope.
func grantScopeFingerprint(cwd string, resources []string) string {
	hash := sha256.New()
	writeFingerprintField(hash, cwd)
	for _, resource := range resources {
		writeFingerprintField(hash, resource)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizedGrantResources(
	resources []tool.Resource, network bool,
) ([]string, uint8) {
	var values []string
	var kinds uint8
	for _, resource := range resources {
		switch resource.Kind {
		case "agent":
			kinds |= 2
		case "sandbox":
			kinds |= 4
		}
		value := resource.Path
		if value == "" {
			value = resource.ID
		}
		if value == "" || resource.Kind == "parallel" {
			continue
		}
		if network {
			target, ok := ParseNetworkTarget(value)
			if resource.Kind != "host" && resource.Kind != "url" || !ok {
				continue
			}
			value = target.Protocol + "://" + target.Host
		} else {
			canonical := cleanGrantPath(value)
			value = resource.Kind + ":" + canonical + ":" + string(resource.Access)
			if resource.Tree || resource.Protocol != "" || resource.Port != 0 ||
				len(resource.Methods) != 0 || resource.AllowPrivate {
				identity := resource
				identity.Path, identity.ID = "", canonical
				identity.Protocol = strings.ToLower(identity.Protocol)
				identity.Methods = append([]string(nil), identity.Methods...)
				sort.Strings(identity.Methods)
				sum := sha256.Sum256([]byte(identity.Key()))
				value += ":" + hex.EncodeToString(sum[:])
			}
		}
		values = append(values, value)
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result, kinds
}

func cleanGrantPath(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return "."
	}
	return filepath.ToSlash(filepath.Clean(value))
}
