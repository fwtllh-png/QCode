package protocol

import (
	"errors"
	"fmt"
	"strings"
)

const SessionToolCatalogVersion = 1

type SessionToolCatalogEntry struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	SourceKind         string `json:"source_kind"`
	SourceLabel        string `json:"source_label"`
	Capability         string `json:"capability"`
	AccessMode         string `json:"access_mode"`
	RiskLevel          string `json:"risk_level"`
	SandboxRequirement string `json:"sandbox_requirement"`
	PolicyState        string `json:"policy_state"`
	PolicyReason       string `json:"policy_reason"`
	ConstitutionState  string `json:"constitution_state"`
	ConstitutionReason string `json:"constitution_reason"`
	Availability       string `json:"availability"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
	State              string `json:"state"`
	Revision           uint64 `json:"revision"`
	Enabled            bool   `json:"enabled"`
	Guarded            bool   `json:"guarded"`
}

type SessionToolCatalog struct {
	Version    int                       `json:"version"`
	CatalogID  string                    `json:"catalog_id"`
	Generation uint64                    `json:"generation"`
	Digest     string                    `json:"digest"`
	Tools      []SessionToolCatalogEntry `json:"tools"`
}

func (c SessionToolCatalog) Validate() error {
	if c.Version != SessionToolCatalogVersion {
		return fmt.Errorf("unsupported session tool catalog version %d", c.Version)
	}
	if c.CatalogID == "" || c.Generation == 0 || c.Digest == "" {
		return errors.New("session tool catalog identity is incomplete")
	}
	if len(c.Tools) > 4096 {
		return errors.New("session tool catalog accepts at most 4096 tools")
	}
	seen := make(map[string]struct{}, len(c.Tools))
	for index, entry := range c.Tools {
		if !validProfileIdentifier(entry.ID) || entry.Name == "" ||
			len(entry.Name) > 256 || strings.ContainsAny(entry.Name, "\x00\r\n") ||
			entry.Description == "" || len(entry.Description) > 4096 ||
			len(entry.UnavailableReason) > 4096 || entry.Revision == 0 {
			return fmt.Errorf("session tool catalog entry %d is incomplete", index)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("session tool catalog entry %q is duplicated", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		switch entry.SourceKind {
		case "builtin", "mcp", "external", "skill", "dynamic":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid source kind", entry.ID)
		}
		if !strings.HasPrefix(entry.ID, entry.SourceKind+":") {
			return fmt.Errorf("session tool catalog entry %q has unbound identity", entry.ID)
		}
		switch entry.Availability {
		case "available", "unavailable", "deferred":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid availability", entry.ID)
		}
		if entry.Availability == "unavailable" && entry.UnavailableReason == "" {
			return fmt.Errorf("session tool catalog entry %q lacks unavailable reason", entry.ID)
		}
		if !entry.Guarded {
			return fmt.Errorf("session tool catalog entry %q is not guarded", entry.ID)
		}
		switch entry.Capability {
		case "read", "write", "process", "network", "external", "unknown":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid capability", entry.ID)
		}
		switch entry.AccessMode {
		case "read", "write", "tree", "use", "unknown":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid access mode", entry.ID)
		}
		switch entry.RiskLevel {
		case "low", "medium", "high", "critical", "unknown":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid risk level", entry.ID)
		}
		switch entry.SandboxRequirement {
		case "none", "strong", "unknown":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid sandbox requirement", entry.ID)
		}
		switch entry.PolicyState {
		case "allowed", "requires_approval", "denied", "deferred":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid policy state", entry.ID)
		}
		switch entry.ConstitutionState {
		case "allowed", "denied", "deferred":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid constitution state", entry.ID)
		}
		if strings.TrimSpace(entry.PolicyReason) == "" ||
			strings.TrimSpace(entry.ConstitutionReason) == "" ||
			len(entry.PolicyReason) > 4096 ||
			len(entry.ConstitutionReason) > 4096 ||
			strings.ContainsRune(entry.PolicyReason, '\x00') ||
			strings.ContainsRune(entry.ConstitutionReason, '\x00') {
			return fmt.Errorf("session tool catalog entry %q has invalid decision reason", entry.ID)
		}
		switch entry.State {
		case "eager", "deferred", "materialized", "unavailable", "revoked":
		default:
			return fmt.Errorf("session tool catalog entry %q has invalid state", entry.ID)
		}
		if len(entry.SourceLabel) == 0 || len(entry.SourceLabel) > 256 ||
			strings.ContainsAny(entry.SourceLabel, "\x00\r\n") ||
			strings.ContainsRune(entry.Description, '\x00') ||
			strings.ContainsRune(entry.UnavailableReason, '\x00') {
			return fmt.Errorf("session tool catalog entry %q has invalid display text", entry.ID)
		}
	}
	return nil
}
