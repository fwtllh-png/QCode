package authority

import (
	"errors"
	"path/filepath"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
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
			ID: input.Tool, Access: securitymodel.Write,
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
	Enforcement        sandbox.Enforcement
	Backend            string
	Controls           securitymodel.Controls
}

func BuildManagedProcessProfile(
	input ManagedProfileInput,
) (EffectivePermissionProfile, error) {
	if input.Controls == (securitymodel.Controls{}) && input.Enforcement == sandbox.EnforcementNone {
		input.Controls = unrestrictedControls()
	}
	if !input.AllowNetwork {
		input.Controls.Network = securitymodel.NetworkDenied
	}
	proxyPort := uint16(0)
	if input.Controls.Network == securitymodel.NetworkProxyTargets {
		proxyPort = input.ManagedProxyPort
	}
	profile := EffectivePermissionProfile{
		SchemaVersion: SchemaVersion, Revision: input.Revision,
		Tool: input.Operation.Tool, Capability: securitymodel.CapabilityProcess,
		Access: securitymodel.Read,
		Filesystem: FilesystemAuthority{
			WorkspaceRoot: input.WorkspaceRoot,
			ReadRoots: append(
				[]string{input.WorkspaceRoot},
				input.ReadRoots...,
			),
			WorkspaceBaseWrite: input.WorkspaceBaseWrite,
		},
		Network: NetworkAuthority{
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

func ManagedProcessEffect(risk securitymodel.Risk) EffectContract {
	return EffectContract{
		Kind:          securitymodel.ProcessReadOnly,
		Reversibility: securitymodel.Bounded,
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
