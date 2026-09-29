package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type SourceKind string

const (
	SourceBuiltin  SourceKind = "builtin"
	SourceSkill    SourceKind = "skill"
	SourceMCP      SourceKind = "mcp"
	SourceExternal SourceKind = "external"
	SourceDynamic  SourceKind = "dynamic"
)

func (s SourceKind) Valid() bool {
	switch s {
	case SourceBuiltin, SourceSkill, SourceMCP, SourceExternal, SourceDynamic:
		return true
	}
	return false
}

type SubjectKind string

const (
	SubjectBuiltin        SubjectKind = "builtin"
	SubjectRepositoryHook SubjectKind = "repository_hook"
	SubjectMCPTool        SubjectKind = "mcp_tool"
	SubjectWorkflow       SubjectKind = "workflow"
	SubjectWorker         SubjectKind = "worker"
	SubjectHost           SubjectKind = "host"
)

type TrustLevel string

const (
	TrustBuiltin   TrustLevel = "builtin"
	TrustHost      TrustLevel = "host"
	TrustWorkspace TrustLevel = "workspace"
	TrustExternal  TrustLevel = "external"
)

type Subject struct {
	Kind       SubjectKind `json:"kind"`
	ID         string      `json:"id"`
	Trust      TrustLevel  `json:"trust"`
	Digest     string      `json:"digest"`
	Generation uint64      `json:"generation"`
}

// PreparedInvocation is the frozen security projection of an executable invocation.
// It contains no executor, presentation descriptor, or adapter resources.
type PreparedInvocation struct {
	CallID, Tool string
	Arguments    json.RawMessage
	Assessment   Assessment
	Subject      Subject
	Required     RequiredControls
}

func (s Subject) Validate() error {
	if s.Kind == "" || strings.TrimSpace(s.ID) == "" ||
		s.Trust == "" || !validSubjectDigest(s.Digest) || s.Generation == 0 {
		return errors.New("execution subject is incomplete")
	}
	switch s.Kind {
	case SubjectBuiltin, SubjectRepositoryHook, SubjectMCPTool,
		SubjectWorkflow, SubjectWorker, SubjectHost:
	default:
		return errors.New("execution subject kind is invalid")
	}
	switch s.Trust {
	case TrustBuiltin, TrustHost, TrustWorkspace, TrustExternal:
	default:
		return errors.New("execution subject trust is invalid")
	}
	return nil
}

func validSubjectDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
