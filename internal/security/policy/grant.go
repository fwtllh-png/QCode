package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
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
	assessment := call.Assessment
	if !assessment.Valid() {
		return Grant{}, false
	}
	resources, agent := grantResources(assessment.Resources())
	summaries := make([]string, 0, len(resources))
	for _, item := range resources {
		summaries = append(summaries, item.String())
	}
	kind, summary := "", ""
	hash := sha256.New()
	writeFingerprintField(hash, call.Tool)
	var prefix []string
	var cwd string
	switch {
	case call.Capability() == securitymodel.CapabilityProcess:
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
		if input.Command == "" {
			return Grant{}, false
		}
		commandIdentity, ok := commandGrantIdentity(input.Command)
		if !ok {
			return Grant{}, false
		}
		prefix = commandGrantPrefix(input.Command)
		kind, summary = "shell", "command: "+input.Command
		writeFingerprintField(hash, commandIdentity)
		cwd = cleanGrantPath(input.CWD)
		writeFingerprintField(hash, cwd)
	case call.Journaled() && len(resources) != 0:
		kind, summary = "file", "workspace paths: "+strings.Join(summaries, ", ")
	case call.Capability() == securitymodel.CapabilityNetwork:
		endpoints := networkGrantEndpoints(resources)
		if len(endpoints) == 0 {
			return Grant{}, false
		}
		kind, summary = "network", "network endpoints: "+strings.Join(endpoints, ", ")
		for _, endpoint := range endpoints {
			writeFingerprintField(hash, endpoint)
		}
		resources = nil
	case agent && len(resources) != 0:
		kind, summary = "agent", call.Tool+": "+strings.Join(summaries, ", ")
	default:
		return Grant{}, false
	}
	keys := make([]string, 0, len(resources))
	for _, item := range resources {
		keys = append(keys, item.Key())
	}
	for _, key := range keys {
		writeFingerprintField(hash, key)
	}
	// Key also matches deny rules, so it must not narrow when facets change;
	// only the prefix scope binds the assessed effect.
	return Grant{
		Kind: kind, Key: hex.EncodeToString(hash.Sum(nil)),
		Summary: summary, Prefix: prefix,
		scope: grantScopeFingerprint(cwd, keys, assessment.Digest()),
	}, true
}

// grantScopeFingerprint binds a shell grant prefix to its cwd, resource set,
// and assessed effect: an approved prefix may only match later commands in
// the same scope with the same effect and facets.
func grantScopeFingerprint(cwd string, resources []string, digest string) string {
	hash := sha256.New()
	writeFingerprintField(hash, cwd)
	for _, resource := range resources {
		writeFingerprintField(hash, resource)
	}
	writeFingerprintField(hash, digest)
	return hex.EncodeToString(hash.Sum(nil))
}

// grantResources returns the addressable resources a grant binds, with
// canonical paths, sorted and deduplicated by Key, and whether any is an agent.
func grantResources(
	resources []securitymodel.Resource,
) ([]securitymodel.Resource, bool) {
	agent := false
	values := make([]securitymodel.Resource, 0, len(resources))
	for _, item := range resources {
		if item.Location() == "" {
			continue
		}
		if item.Class == securitymodel.ClassAgent {
			agent = true
		}
		if item.Class == securitymodel.ClassPath {
			item.Path = cleanGrantPath(item.Path)
		}
		values = append(values, item)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Key() < values[j].Key() })
	return slices.CompactFunc(values, func(a, b securitymodel.Resource) bool {
		return a.Key() == b.Key()
	}), agent
}

// networkGrantEndpoints is the scheme://host set of a network grant; ports,
// methods, and paths are enforced per request.
func networkGrantEndpoints(resources []securitymodel.Resource) []string {
	var endpoints []string
	for _, item := range resources {
		switch {
		case item.Class == securitymodel.ClassLoopback:
			endpoints = append(endpoints,
				securitymodel.LoopbackScope)
		case item.Class == securitymodel.ClassNetwork && item.Network != nil:
			endpoints = append(endpoints, item.Network.Scheme+"://"+item.Network.Host)
		}
	}
	slices.Sort(endpoints)
	return slices.Compact(endpoints)
}

func cleanGrantPath(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return "."
	}
	return filepath.ToSlash(filepath.Clean(value))
}
