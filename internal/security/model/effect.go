package model

import (
	"errors"
	"fmt"
)

type EffectKind string

const (
	WorkspaceRead    EffectKind = "workspace.read"
	WorkspaceEdit    EffectKind = "workspace.edit"
	ProcessReadOnly  EffectKind = "process.read_only"
	ProcessMutating  EffectKind = "process.mutating"
	NetworkRead      EffectKind = "network.read"
	NetworkMutating  EffectKind = "network.mutating"
	SessionMutation  EffectKind = "session.mutation"
	AgentMessage     EffectKind = "agent.message"
	AgentLifecycle   EffectKind = "agent.lifecycle"
	ExternalMutation EffectKind = "external.mutation"
)

func (k EffectKind) Valid() bool {
	switch k {
	case WorkspaceRead, WorkspaceEdit, ProcessReadOnly, ProcessMutating,
		NetworkRead, NetworkMutating, SessionMutation, AgentMessage,
		AgentLifecycle, ExternalMutation:
		return true
	}
	return false
}

type Risk string

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

func (r Risk) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

type Reversibility string

const (
	Reversible   Reversibility = "reversible"
	Bounded      Reversibility = "bounded"
	Irreversible Reversibility = "irreversible"
)

func (r Reversibility) Valid() bool {
	switch r {
	case Reversible, Bounded, Irreversible:
		return true
	}
	return false
}

// Capability is the broadest authority class a tool binding declares.
type Capability string

type ApprovalMode string

const (
	ApprovalDefault ApprovalMode = "default"
	ApprovalOnce    ApprovalMode = "once_required"
)

const (
	CapabilityRead     Capability = "read"
	CapabilityWrite    Capability = "write"
	CapabilityProcess  Capability = "process"
	CapabilityNetwork  Capability = "network"
	CapabilityExternal Capability = "external"
)

// Effect is the classified consequence of one operation.
type Effect struct {
	Kind          EffectKind
	Risk          Risk
	Reversibility Reversibility
}

func (e Effect) Validate() error {
	if !e.Kind.Valid() {
		return fmt.Errorf("effect kind %q is invalid", e.Kind)
	}
	if !e.Risk.Valid() {
		return fmt.Errorf("effect risk %q is invalid", e.Risk)
	}
	if !e.Reversibility.Valid() {
		return errors.New("effect reversibility is invalid")
	}
	return nil
}
