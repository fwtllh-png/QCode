package authority

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

const OperationSchemaVersion = 3

type SubjectKind = securitymodel.SubjectKind

type TrustLevel = securitymodel.TrustLevel

type Subject = securitymodel.Subject

const (
	SubjectBuiltin        = securitymodel.SubjectBuiltin
	SubjectRepositoryHook = securitymodel.SubjectRepositoryHook
	SubjectMCPTool        = securitymodel.SubjectMCPTool
	SubjectWorkflow       = securitymodel.SubjectWorkflow
	SubjectWorker         = securitymodel.SubjectWorker
	SubjectHost           = securitymodel.SubjectHost
	TrustBuiltin          = securitymodel.TrustBuiltin
	TrustHost             = securitymodel.TrustHost
	TrustWorkspace        = securitymodel.TrustWorkspace
	TrustExternal         = securitymodel.TrustExternal
)

type ResourceNamespace string

const (
	NamespaceWorkspace      ResourceNamespace = "workspace"
	NamespaceSandboxHome    ResourceNamespace = "sandbox_home"
	NamespaceBrokerArtifact ResourceNamespace = "broker_artifact"
	NamespaceHostToolchain  ResourceNamespace = "host_toolchain"
	NamespaceHostConfig     ResourceNamespace = "host_config"
	NamespaceCache          ResourceNamespace = "cache"
	NamespaceSharedUserTemp ResourceNamespace = "shared_user_temp"
	NamespaceCredential     ResourceNamespace = "credential"
	NamespaceControlState   ResourceNamespace = "control_state"
	NamespaceNetwork        ResourceNamespace = "network"
	NamespaceProcess        ResourceNamespace = "process"
	NamespaceRuntime        ResourceNamespace = "runtime"
)

type Resource struct {
	Namespace      ResourceNamespace    `json:"namespace"`
	RootID         string               `json:"root_id,omitempty"`
	RelativePath   string               `json:"relative_path,omitempty"`
	RootGeneration uint64               `json:"root_generation,omitempty"`
	FileIdentity   string               `json:"file_identity,omitempty"`
	Kind           string               `json:"kind"`
	ID             string               `json:"id,omitempty"`
	Access         securitymodel.Access `json:"access"`
	Tree           bool                 `json:"tree,omitempty"`
	Protocol       string               `json:"protocol,omitempty"`
	Port           uint16               `json:"port,omitempty"`
	Methods        []string             `json:"methods,omitempty"`
	AllowPrivate   bool                 `json:"allow_private,omitempty"`
}

type WorkspaceTransaction string

const (
	WorkspaceTransactionNone        WorkspaceTransaction = "none"
	WorkspaceTransactionBeforeImage WorkspaceTransaction = "before_image"
)

type EffectContract struct {
	Kind                   securitymodel.EffectKind    `json:"kind"`
	Reversibility          securitymodel.Reversibility `json:"reversibility"`
	Risk                   securitymodel.Risk          `json:"risk"`
	WorkspaceTransaction   WorkspaceTransaction        `json:"workspace_transaction"`
	RequireReadBeforeWrite bool                        `json:"require_read_before_write,omitempty"`
}

type RequiredControls = securitymodel.RequiredControls

type ProcessIntent struct {
	Kind            string `json:"kind"`
	Tool            string `json:"tool"`
	ArgumentsDigest string `json:"arguments_digest"`
}

type NetworkIntent struct {
	Targets     []string `json:"targets,omitempty"`
	LoopbackAny bool     `json:"loopback_any,omitempty"`
}

type FileIntent struct {
	ResourceDigests []string `json:"resource_digests"`
	MutationDigest  string   `json:"mutation_digest,omitempty"`
}

type ArtifactIntent struct {
	ManifestDigest string `json:"manifest_digest"`
	Generation     uint64 `json:"generation"`
}

type ExecutionOperation struct {
	SchemaVersion       int              `json:"schema_version"`
	ID                  string           `json:"id"`
	Tool                string           `json:"tool"`
	WorkspaceID         string           `json:"workspace_id"`
	WorkspaceGeneration uint64           `json:"workspace_generation"`
	Subject             Subject          `json:"subject"`
	Effect              EffectContract   `json:"effect"`
	Required            RequiredControls `json:"required_controls"`
	Resources           []Resource       `json:"resources"`
	Process             *ProcessIntent   `json:"process,omitempty"`
	Network             *NetworkIntent   `json:"network,omitempty"`
	File                *FileIntent      `json:"file,omitempty"`
	Artifact            *ArtifactIntent  `json:"artifact,omitempty"`
	Digest              string           `json:"digest"`
}

type operationInput struct {
	WorkspaceRoot          string
	WorkspaceID            string
	WorkspaceGeneration    uint64
	Invocation             securitymodel.PreparedInvocation
	Effect                 securitymodel.Effect
	Journaled              bool
	RequireReadBeforeWrite bool
	Required               RequiredControls
	Artifact               *ArtifactIntent
	FileMutationDigest     string
	HostReadRoots          []string
}

func buildExecutionOperation(
	input operationInput,
	sources []securitymodel.Resource,
) (ExecutionOperation, error) {
	workspaceRoot, err := filepath.Abs(input.WorkspaceRoot)
	if err != nil {
		return ExecutionOperation{}, fmt.Errorf("resolve operation workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(workspaceRoot); resolveErr == nil {
		workspaceRoot = resolved
	}
	if strings.TrimSpace(input.WorkspaceID) == "" {
		input.WorkspaceID = digestString(filepath.Clean(workspaceRoot))
	}
	if input.WorkspaceGeneration == 0 {
		input.WorkspaceGeneration = 1
	}
	resources := make([]Resource, 0, len(sources))
	for _, source := range sources {
		resource, normalizeErr := normalizeResource(
			workspaceRoot,
			input.WorkspaceID,
			input.WorkspaceGeneration,
			input.HostReadRoots,
			source,
		)
		if normalizeErr != nil {
			return ExecutionOperation{}, normalizeErr
		}
		resources = append(resources, resource)
	}
	resources = normalizeResources(resources)
	contract := EffectContract{
		Kind: input.Effect.Kind, Risk: input.Effect.Risk,
		Reversibility:          input.Effect.Reversibility,
		WorkspaceTransaction:   WorkspaceTransactionNone,
		RequireReadBeforeWrite: input.RequireReadBeforeWrite,
	}
	if input.Journaled {
		contract.WorkspaceTransaction = WorkspaceTransactionBeforeImage
	}
	operation := ExecutionOperation{
		SchemaVersion: OperationSchemaVersion,
		ID:            input.Invocation.CallID, Tool: input.Invocation.Tool,
		WorkspaceID:         input.WorkspaceID,
		WorkspaceGeneration: input.WorkspaceGeneration,
		Subject:             input.Invocation.Subject, Effect: contract, Required: input.Required,
		Resources: resources, Artifact: cloneArtifactIntent(input.Artifact),
	}
	argumentsDigest, err := canonicalJSONDigest(input.Invocation.Arguments)
	if err != nil {
		return ExecutionOperation{}, fmt.Errorf("digest operation arguments: %w", err)
	}
	switch input.Invocation.Assessment.Binding().Capability {
	case securitymodel.CapabilityProcess, securitymodel.CapabilityExternal:
		operation.Process = &ProcessIntent{
			Kind: "tool", Tool: input.Invocation.Tool,
			ArgumentsDigest: argumentsDigest,
		}
	}
	var networkTargets, fileDigests []string
	loopback := false
	for _, resource := range resources {
		digest, digestErr := resourceDigest(resource)
		if digestErr != nil {
			return ExecutionOperation{}, digestErr
		}
		switch resource.Namespace {
		case NamespaceNetwork:
			if resource.Kind == securitymodel.ClassLoopback.String() {
				loopback = true
			} else {
				networkTargets = append(networkTargets, resource.ID)
			}
		case NamespaceWorkspace, NamespaceSandboxHome, NamespaceBrokerArtifact,
			NamespaceHostToolchain, NamespaceCache, NamespaceSharedUserTemp,
			NamespaceControlState:
			fileDigests = append(fileDigests, digest)
		case NamespaceHostConfig:
			if resource.Kind != "env" {
				fileDigests = append(fileDigests, digest)
			}
		}
	}
	if len(networkTargets) != 0 || loopback {
		operation.Network = &NetworkIntent{Targets: uniqueSorted(networkTargets), LoopbackAny: loopback}
	}
	if len(fileDigests) != 0 {
		operation.File = &FileIntent{
			ResourceDigests: uniqueSorted(fileDigests),
			MutationDigest:  input.FileMutationDigest,
		}
	}
	digest, err := operationDigest(operation)
	if err != nil {
		return ExecutionOperation{}, err
	}
	operation.Digest = digest
	return operation, operation.Validate()
}

func (o ExecutionOperation) Validate() error {
	if o.SchemaVersion != OperationSchemaVersion ||
		strings.TrimSpace(o.ID) == "" ||
		strings.TrimSpace(o.Tool) == "" ||
		!validDigest(o.WorkspaceID) ||
		o.WorkspaceGeneration == 0 {
		return errors.New("execution operation identity is incomplete")
	}
	if err := o.Subject.Validate(); err != nil {
		return err
	}
	if err := o.Effect.Validate(); err != nil {
		return err
	}
	if err := o.Required.Validate(); err != nil {
		return fmt.Errorf("required controls: %w", err)
	}
	for _, resource := range o.Resources {
		if err := resource.Validate(); err != nil {
			return err
		}
	}
	if !sort.SliceIsSorted(o.Resources, func(i, j int) bool {
		return resourceKey(o.Resources[i]) < resourceKey(o.Resources[j])
	}) {
		return errors.New("execution operation resources are not normalized")
	}
	for index := 1; index < len(o.Resources); index++ {
		if resourceKey(o.Resources[index-1]) == resourceKey(o.Resources[index]) {
			return errors.New("execution operation resources are duplicated")
		}
	}
	if o.Process != nil &&
		(o.Process.Kind != "tool" ||
			o.Process.Tool != o.Tool ||
			!validDigest(o.Process.ArgumentsDigest)) {
		return errors.New("execution process intent is invalid")
	}
	if o.Network != nil {
		if (len(o.Network.Targets) == 0 && !o.Network.LoopbackAny) ||
			!sort.StringsAreSorted(o.Network.Targets) {
			return errors.New("execution network intent is invalid")
		}
		for index, target := range o.Network.Targets {
			if strings.TrimSpace(target) == "" ||
				(index > 0 && target == o.Network.Targets[index-1]) {
				return errors.New("execution network targets are invalid")
			}
		}
	}
	if o.File != nil {
		if len(o.File.ResourceDigests) == 0 ||
			!sort.StringsAreSorted(o.File.ResourceDigests) {
			return errors.New("execution file intent is invalid")
		}
		for index, digest := range o.File.ResourceDigests {
			if !validDigest(digest) ||
				(index > 0 && digest == o.File.ResourceDigests[index-1]) {
				return errors.New("execution file resource digest is invalid")
			}
		}
		if o.File.MutationDigest != "" && !validDigest(o.File.MutationDigest) {
			return errors.New("execution file mutation digest is invalid")
		}
	}
	if o.Artifact != nil &&
		(!validDigest(o.Artifact.ManifestDigest) || o.Artifact.Generation == 0) {
		return errors.New("execution artifact intent is invalid")
	}
	expected, err := operationDigest(o)
	if err != nil {
		return err
	}
	if !validDigest(o.Digest) || o.Digest != expected {
		return errors.New("execution operation digest mismatch")
	}
	return nil
}

func (e EffectContract) Validate() error {
	switch e.WorkspaceTransaction {
	case WorkspaceTransactionNone, WorkspaceTransactionBeforeImage:
	default:
		return errors.New("workspace transaction is invalid")
	}
	if err := (securitymodel.Effect{
		Kind: e.Kind, Risk: e.Risk, Reversibility: e.Reversibility,
	}).Validate(); err != nil {
		return err
	}
	if e.RequireReadBeforeWrite &&
		e.WorkspaceTransaction != WorkspaceTransactionBeforeImage {
		return errors.New("read-before-write requires a workspace transaction")
	}
	return nil
}

func (r Resource) Validate() error {
	if r.Namespace == "" || strings.TrimSpace(r.Kind) == "" || r.Access == "" {
		return errors.New("operation resource is incomplete")
	}
	switch r.Namespace {
	case NamespaceWorkspace, NamespaceSandboxHome, NamespaceBrokerArtifact,
		NamespaceHostToolchain, NamespaceHostConfig, NamespaceCache,
		NamespaceSharedUserTemp, NamespaceCredential, NamespaceControlState,
		NamespaceNetwork, NamespaceProcess, NamespaceRuntime:
	default:
		return errors.New("operation resource namespace is invalid")
	}
	switch r.Access {
	case securitymodel.Read, securitymodel.Write, securitymodel.Tree, securitymodel.Use:
	default:
		return errors.New("operation resource access is invalid")
	}
	if r.Access == securitymodel.Use && r.Namespace != NamespaceCredential {
		return errors.New("access use is only valid for credential resources")
	}
	if r.Namespace == NamespaceCredential && r.Access != securitymodel.Use {
		return errors.New("credential resources must use access use")
	}
	if r.Namespace == NamespaceCredential && strings.TrimSpace(r.ID) == "" {
		return errors.New("credential resource identity is required")
	}
	pathNamespace := r.bindsFilesystem()
	if pathNamespace {
		if r.RelativePath == "" {
			return errors.New("operation resource relative path is required")
		}
		if filepath.IsAbs(r.RelativePath) ||
			r.RelativePath == ".." ||
			strings.HasPrefix(r.RelativePath, ".."+string(filepath.Separator)) {
			return errors.New("operation resource path is not relative")
		}
		if !validDigest(r.RootID) || r.RootGeneration == 0 {
			return errors.New("operation resource root binding is incomplete")
		}
	} else if r.RelativePath != "" || r.RootID != "" || r.RootGeneration != 0 {
		return errors.New("non-filesystem resource carries a root binding")
	}
	if r.Namespace == NamespaceNetwork && strings.TrimSpace(r.ID) == "" {
		return errors.New("network resource target is required")
	}
	if r.Namespace == NamespaceHostConfig && r.Kind == "env" &&
		strings.TrimSpace(r.ID) == "" {
		return errors.New("host_config env resource identity is required")
	}
	return nil
}

func (r Resource) bindsFilesystem() bool {
	switch r.Namespace {
	case NamespaceWorkspace, NamespaceSandboxHome, NamespaceBrokerArtifact,
		NamespaceHostToolchain, NamespaceCache, NamespaceSharedUserTemp,
		NamespaceControlState:
		return true
	case NamespaceHostConfig:
		return r.Kind != "env"
	default:
		return false
	}
}

func normalizeResource(
	workspaceRoot, workspaceID string,
	workspaceGeneration uint64,
	hostReadRoots []string,
	source securitymodel.Resource,
) (Resource, error) {
	resource := Resource{
		Kind: source.Class.String(), ID: strings.TrimSpace(source.ID),
		Access: source.Access, Tree: source.Tree,
		AllowPrivate: source.AllowPrivate,
	}
	resource.Methods = append([]string(nil), source.Methods...)
	for index := range resource.Methods {
		resource.Methods[index] = strings.ToUpper(strings.TrimSpace(resource.Methods[index]))
	}
	resource.Methods = uniqueSorted(resource.Methods)
	switch source.Class {
	case securitymodel.ClassLoopback:
		resource.Namespace = NamespaceNetwork
		resource.ID = securitymodel.LoopbackScope
		resource.Protocol = securitymodel.LoopbackProtocol
	case securitymodel.ClassNetwork:
		resource.Namespace = NamespaceNetwork
		if source.Network == nil || source.Network.Scheme == "" || source.Network.Host == "" {
			return Resource{}, errors.New("operation network target is invalid")
		}
		resource.ID = source.Network.Key()
		resource.Protocol, resource.Port = source.Network.Scheme, source.Network.Port
	case securitymodel.ClassProcess:
		resource.Namespace = NamespaceProcess
	case securitymodel.ClassAgent, securitymodel.ClassPlan,
		securitymodel.ClassSession:
		resource.Namespace = NamespaceRuntime
	case securitymodel.ClassNamed:
		resource.Namespace, resource.Kind = NamespaceRuntime, source.Name
	case securitymodel.ClassPath:
		if strings.TrimSpace(source.Path) == "" {
			resource.Namespace = NamespaceRuntime
			break
		}
		path, err := filepath.Abs(source.Path)
		if err != nil {
			return Resource{}, fmt.Errorf("resolve operation resource path: %w", err)
		}
		path, err = pathpolicy.CanonicalAllowMissing(path)
		if err != nil {
			return Resource{}, fmt.Errorf(
				"canonicalize operation resource path: %w",
				err,
			)
		}
		relative, relErr := filepath.Rel(workspaceRoot, path)
		if relErr == nil && relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			resource.Namespace = NamespaceWorkspace
			resource.RootID = workspaceID
			resource.RootGeneration = workspaceGeneration
			resource.RelativePath = filepath.Clean(relative)
		} else {
			hostRoot, rootErr := authorizedHostRoot(path, hostReadRoots)
			if rootErr != nil {
				return Resource{}, rootErr
			}
			resource.Namespace = NamespaceHostToolchain
			resource.RootID = digestString(hostRoot)
			resource.RootGeneration = 1
			resource.RelativePath, err = filepath.Rel(hostRoot, path)
			if err != nil {
				return Resource{}, err
			}
		}
		resource.FileIdentity = fileIdentity(path)
	default:
		return Resource{}, errors.New("operation resource class is invalid")
	}
	if err := resource.Validate(); err != nil {
		return Resource{}, err
	}
	return resource, nil
}

func authorizedHostRoot(path string, roots []string) (string, error) {
	var selected string
	for _, candidate := range roots {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		root, err := pathpolicy.CanonicalAllowMissing(candidate)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(selected) {
			selected = root
		}
	}
	if selected == "" {
		return "", errors.New(
			"operation resource path is outside authorized namespaces",
		)
	}
	return selected, nil
}

func fileIdentity(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return digestString(resolved)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return ""
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func normalizeResources(resources []Resource) []Resource {
	sort.Slice(resources, func(i, j int) bool {
		return resourceKey(resources[i]) < resourceKey(resources[j])
	})
	result := resources[:0]
	for _, resource := range resources {
		if len(result) == 0 || resourceKey(result[len(result)-1]) != resourceKey(resource) {
			result = append(result, resource)
		}
	}
	return result
}

func resourceKey(resource Resource) string {
	encoded, _ := json.Marshal(resource)
	return string(encoded)
}

func resourceDigest(resource Resource) (string, error) {
	return digestValue(resource)
}

func operationDigest(operation ExecutionOperation) (string, error) {
	operation.Digest = ""
	return digestValue(operation)
}

func canonicalJSONDigest(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("operation arguments contain trailing data")
	}
	return digestValue(value)
}

func digestValue(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func DigestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func digestString(value string) string {
	return DigestString(value)
}

func FallbackSandboxPolicyID(
	workspaceRoot, backend string,
	controls securitymodel.Controls,
) string {
	if strings.TrimSpace(workspaceRoot) == "" ||
		strings.TrimSpace(backend) == "" {
		return ""
	}
	digest, err := digestValue(struct {
		WorkspaceRoot string
		Backend       string
		Controls      securitymodel.Controls
	}{
		WorkspaceRoot: filepath.Clean(workspaceRoot),
		Backend:       backend,
		Controls:      controls,
	})
	if err != nil {
		return ""
	}
	return digest
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneArtifactIntent(source *ArtifactIntent) *ArtifactIntent {
	if source == nil {
		return nil
	}
	cloned := *source
	return &cloned
}
