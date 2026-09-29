package authority

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/effect"
)

type ManagedProcessInput struct {
	ID                  string
	Tool                string
	WorkspaceID         string
	WorkspaceGeneration uint64
	Subject             Subject
	Executable          string
	Args                []string
	WorkingDirectory    string
	Environment         []string
	Effect              EffectContract
	Required            RequiredControls
}

func BuildManagedProcessOperation(
	input ManagedProcessInput,
) (ExecutionOperation, error) {
	if err := input.Subject.Validate(); err != nil {
		return ExecutionOperation{}, err
	}
	if strings.TrimSpace(input.ID) == "" ||
		strings.TrimSpace(input.Tool) == "" ||
		!validDigest(input.WorkspaceID) ||
		input.WorkspaceGeneration == 0 ||
		strings.TrimSpace(input.Executable) == "" ||
		strings.TrimSpace(input.WorkingDirectory) == "" {
		return ExecutionOperation{}, errors.New("managed process operation is incomplete")
	}
	argumentsDigest, err := ManagedProcessArgumentsDigest(
		input.Executable,
		input.Args,
		input.Environment,
		input.WorkingDirectory,
	)
	if err != nil {
		return ExecutionOperation{}, err
	}
	operation := ExecutionOperation{
		SchemaVersion: OperationSchemaVersion,
		ID:            input.ID, Tool: input.Tool,
		WorkspaceID:         input.WorkspaceID,
		WorkspaceGeneration: input.WorkspaceGeneration,
		Subject:             input.Subject, Effect: input.Effect, Required: input.Required,
		Resources: []Resource{{
			Namespace: NamespaceProcess, Kind: "process",
			ID: input.Tool, Access: tool.AccessWrite,
		}},
		Process: &ProcessIntent{
			Kind: "tool", Tool: input.Tool,
			ArgumentsDigest: argumentsDigest,
		},
	}
	digest, err := operationDigest(operation)
	if err != nil {
		return ExecutionOperation{}, err
	}
	operation.Digest = digest
	return operation, operation.Validate()
}

func NewManagedProcessSubject(
	kind SubjectKind,
	id string,
	trust TrustLevel,
	generation uint64,
	material any,
) (Subject, error) {
	digest, err := digestValue(material)
	if err != nil {
		return Subject{}, err
	}
	subject := Subject{
		Kind: kind, ID: strings.TrimSpace(id), Trust: trust,
		Digest: digest, Generation: generation,
	}
	return subject, subject.Validate()
}

type ManagedProfileInput struct {
	Operation          ExecutionOperation
	Revision           uint64
	WorkspaceRoot      string
	WorkspaceBaseWrite bool
	ReadRoots          []string
	AllowNetwork       bool
	NetworkTargets     []string
	ManagedProxyPort   uint16
	Enforcement        string
	Backend            string
	Controls           controlmatrix.Matrix
}

func BuildManagedProcessProfile(
	input ManagedProfileInput,
) (EffectivePermissionProfile, error) {
	if input.Controls == (controlmatrix.Matrix{}) && input.Enforcement == "none" {
		input.Controls = unrestrictedControls()
	}
	networkMode := "denied"
	proxyPort := uint16(0)
	if input.AllowNetwork {
		switch {
		case input.ManagedProxyPort != 0:
			networkMode = "managed"
			proxyPort = input.ManagedProxyPort
		case input.Enforcement == "none":
			networkMode = "unrestricted"
		default:
			networkMode = "direct"
		}
	}
	profile := EffectivePermissionProfile{
		SchemaVersion: SchemaVersion, Revision: input.Revision,
		Tool: input.Operation.Tool, Capability: tool.CapabilityProcess,
		Access: tool.AccessRead,
		Filesystem: FilesystemAuthority{
			WorkspaceRoot: input.WorkspaceRoot,
			ReadRoots: append(
				[]string{input.WorkspaceRoot},
				input.ReadRoots...,
			),
			WorkspaceBaseWrite: input.WorkspaceBaseWrite,
		},
		Network: NetworkAuthority{Mode: networkMode,
			Targets:   append([]string(nil), input.NetworkTargets...),
			ProxyPort: proxyPort,
		},
		Process: ProcessAuthority{
			Allowed: true, Enforcement: input.Enforcement,
			Backend: input.Backend,
		},
		Controls: input.Controls,
		Provenance: []AuthoritySource{{
			Kind: "managed_process", Value: input.Operation.Subject.ID,
			Digest:   input.Operation.Subject.Digest,
			Revision: input.Operation.Subject.Generation,
		}},
	}
	normalize(&profile)
	digest, err := profileDigest(profile)
	if err != nil {
		return EffectivePermissionProfile{}, err
	}
	profile.Digest = digest
	return profile, profile.Validate()
}

func ManagedProcessEffect(risk effect.Risk) EffectContract {
	return EffectContract{
		Kind:          effect.ProcessReadOnly,
		Reversibility: effect.Bounded,
		Risk:          risk, WorkspaceTransaction: WorkspaceTransactionNone,
	}
}

func ManagedProcessArgumentsDigest(
	executable string,
	args, environment []string,
	workingDirectory string,
) (string, error) {
	return digestValue(struct {
		Executable       string   `json:"executable"`
		Args             []string `json:"args"`
		WorkingDirectory string   `json:"working_directory"`
		Environment      []string `json:"environment"`
	}{
		Executable: filepath.Clean(executable), Args: args,
		WorkingDirectory: filepath.Clean(workingDirectory),
		Environment:      environment,
	})
}
