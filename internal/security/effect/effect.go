// Package effect is the single vocabulary for what a consequential operation
// does: its kind, risk, and reversibility. Tool descriptors declare it, the
// policy engine derives it, and authority binds it into an operation.
package effect

import (
	"errors"
	"fmt"
)

type Kind string

const (
	WorkspaceRead    Kind = "workspace.read"
	WorkspaceEdit    Kind = "workspace.edit"
	ProcessReadOnly  Kind = "process.read_only"
	ProcessMutating  Kind = "process.mutating"
	NetworkRead      Kind = "network.read"
	NetworkMutating  Kind = "network.mutating"
	SessionMutation  Kind = "session.mutation"
	AgentMessage     Kind = "agent.message"
	AgentLifecycle   Kind = "agent.lifecycle"
	ExternalMutation Kind = "external.mutation"
)

func (k Kind) Valid() bool {
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

// Effect is the classified consequence of one operation.
type Effect struct {
	Kind          Kind
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
