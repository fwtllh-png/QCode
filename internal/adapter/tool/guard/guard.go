package guard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/textdiff"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlplane"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type ApprovalRequest struct {
	RequestID            string                                 `json:"request_id"`
	CallID               string                                 `json:"call_id"`
	Tool                 string                                 `json:"tool"`
	Arguments            json.RawMessage                        `json:"arguments"`
	ArgumentsDigest      string                                 `json:"arguments_digest"`
	Resources            []tool.Resource                        `json:"resources"`
	AllowedScopes        []policy.ApprovalScope                 `json:"allowed_scopes"`
	ExpiresAt            time.Time                              `json:"expires_at"`
	ReplacementAllowed   bool                                   `json:"replacement_allowed"`
	ModifiableArguments  []string                               `json:"modifiable_arguments"`
	Effect               policy.EffectKind                      `json:"effect"`
	Risk                 policy.RiskLevel                       `json:"risk"`
	ReasonCode           string                                 `json:"reason_code"`
	Network              *NetworkApprovalContext                `json:"network,omitempty"`
	EditPlan             *tool.EditPlan                         `json:"edit_plan,omitempty"`
	Grant                *policy.Grant                          `json:"grant,omitempty"`
	AdditionalPermission *authority.AdditionalPermissionRequest `json:"additional_permission,omitempty"`
}

type FileChange = tool.WorkspaceChange

const (
	FileCreated  = tool.WorkspaceCreated
	FileModified = tool.WorkspaceModified
	FileDeleted  = tool.WorkspaceDeleted
)

type NetworkApprovalContext struct {
	Host         string   `json:"host"`
	Protocol     string   `json:"protocol"`
	Port         uint16   `json:"port,omitempty"`
	Methods      []string `json:"methods,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`
	Mode         string   `json:"mode"`
}

// NetworkAllow routes grants using the invocation's trusted capability.
type NetworkAllow func(tool.Capability, egress.Target)

type ApprovalDecision struct {
	RequestID            string
	CallID               string
	Approved             bool
	Canceled             bool // Esc / cancel: abort wait without deny-as-tool-failure
	Scope                policy.ApprovalScope
	ExpiresAt            time.Time
	ReplacementArguments json.RawMessage
	PlanID               string
}

type Invocation = tool.PreparedInvocation

type Options struct {
	Registry              *tool.Registry
	Policy                *policy.Runtime
	Workspace             string
	Approvals             func(context.Context, ApprovalRequest) error
	PersistAllow          func(policy.Invocation) error
	OnNetworkAllow        NetworkAllow
	Now                   func() time.Time
	ApprovalTTL           time.Duration
	LeaseTTL              time.Duration
	ReadTracker           *workspacejournal.ReadTracker
	Journal               *workspacejournal.Manager
	Diagnostics           diagnostics.Runner
	Escalation            *EscalationPolicy
	ForceEditPlanApproval bool
	WorkspaceID           string
	WorkspaceGeneration   uint64
	LeaseAuthority        *authority.LeaseAuthority
}

type pending struct {
	callID   string
	decision chan ApprovalDecision
	resume   chan struct{}
	decided  bool
}

type ApprovalWaitOutcome string

const (
	ApprovalWaitDecided  ApprovalWaitOutcome = "decided"
	ApprovalWaitExpired  ApprovalWaitOutcome = "expired"
	ApprovalWaitCanceled ApprovalWaitOutcome = "canceled"
)

type ApprovalWait struct {
	RequestID string
	CallID    string
	Tool      string
	Waited    time.Duration
	Outcome   ApprovalWaitOutcome
}

type ApprovalObserver func(string, string, string, string, time.Duration)

// DefaultLeaseTTL is the direct Guard-construction fallback. Application
// wiring supplies execution.lease_timeout with configuration provenance.
const DefaultLeaseTTL = 2 * time.Minute

type Guard struct {
	registry              *tool.Registry
	policy                *policy.Runtime
	workspace             string
	controlPlane          *controlplane.Classifier
	approvals             func(context.Context, ApprovalRequest) error
	persistAllow          func(policy.Invocation) error
	onNetworkAllow        NetworkAllow
	now                   func() time.Time
	approvalTTL           time.Duration
	leaseTTL              time.Duration
	readTracker           *workspacejournal.ReadTracker
	journal               *workspacejournal.Manager
	diagnostics           diagnostics.Runner
	escalation            EscalationPolicy
	forceEditPlanApproval bool
	workspaceID           string
	workspaceGeneration   uint64
	leaseAuthority        *authority.LeaseAuthority

	mu           sync.Mutex
	pending      map[string]*pending
	completed    map[string]time.Time
	recovered    map[string]ApprovalRequest
	restoreWait  func(ApprovalRequest) error
	approvalWait func(ApprovalWait)
	expireWait   func(ApprovalWait) error
	observe      ApprovalObserver
}

type approvalAsk struct {
	Code                 string
	AllowedScopes        []policy.ApprovalScope
	DisableReplace       bool
	Network              *NetworkApprovalContext
	EditPlan             *tool.EditPlan
	AdditionalPermission *authority.AdditionalPermissionRequest
}

func New(options Options) (*Guard, error) {
	if options.Registry == nil {
		return nil, errors.New("tool guard registry is required")
	}
	if options.Policy == nil {
		return nil, errors.New("tool guard policy is required")
	}
	if err := policy.Validate(options.Policy); err != nil {
		return nil, err
	}
	workspace := options.Workspace
	if workspace == "" {
		workspace = "."
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve tool guard workspace: %w", err)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ApprovalTTL < 0 {
		return nil, errors.New("tool guard approval TTL must be non-negative")
	}
	if options.LeaseTTL <= 0 {
		options.LeaseTTL = DefaultLeaseTTL
	}
	if options.ReadTracker == nil {
		options.ReadTracker = workspacejournal.NewReadTracker()
	}
	if options.Diagnostics == nil {
		options.Diagnostics = diagnostics.UnavailableRunner{}
	}
	if options.WorkspaceGeneration == 0 {
		options.WorkspaceGeneration = 1
	}
	if options.LeaseAuthority == nil {
		options.LeaseAuthority = authority.NewLeaseAuthority(
			authority.LeaseAuthorityOptions{Now: options.Now},
		)
	}
	escalation := DefaultEscalationPolicy()
	if options.Escalation != nil {
		escalation = *options.Escalation
	}
	for index := range options.Policy.Repository {
		if err := canonicalizeRuleResource(&options.Policy.Repository[index], absolute); err != nil {
			return nil, fmt.Errorf("repository rule: %w", err)
		}
	}
	for index := range options.Policy.Grants {
		if err := canonicalizeRuleResource(&options.Policy.Grants[index], absolute); err != nil {
			return nil, fmt.Errorf("grant rule: %w", err)
		}
	}
	controlPlane, err := controlplane.New(absolute)
	if err != nil {
		return nil, err
	}
	return &Guard{
		registry: options.Registry, policy: options.Policy, workspace: absolute,
		controlPlane: controlPlane,
		approvals:    options.Approvals, persistAllow: options.PersistAllow,
		onNetworkAllow: options.OnNetworkAllow,
		now:            options.Now,
		approvalTTL:    options.ApprovalTTL,
		leaseTTL:       options.LeaseTTL,
		readTracker:    options.ReadTracker, journal: options.Journal, diagnostics: options.Diagnostics,
		escalation:            escalation,
		forceEditPlanApproval: options.ForceEditPlanApproval,
		workspaceID:           options.WorkspaceID,
		workspaceGeneration:   options.WorkspaceGeneration,
		leaseAuthority:        options.LeaseAuthority,
		pending:               make(map[string]*pending), completed: make(map[string]time.Time),
		recovered: make(map[string]ApprovalRequest),
	}, nil
}

func (g *Guard) SetApprovalObserver(observer ApprovalObserver) {
	g.observe = observer
}

func (g *Guard) Execute(
	ctx context.Context, callID, name string, raw json.RawMessage,
) (tool.Result, error) {
	return g.ExecuteBound(ctx, callID, name, raw, tool.CatalogBinding{})
}

func (g *Guard) ExecuteBound(
	ctx context.Context,
	callID, name string,
	raw json.RawMessage,
	binding tool.CatalogBinding,
) (tool.Result, error) {
	if callID == "" {
		return tool.Result{}, errors.New("tool guard call id is required")
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = callID
	ctx = tool.WithInvocationIdentity(ctx, identity)
	return g.executePipeline(ctx, callID, name, raw, binding)
}

func (g *Guard) canEscalate(invocation Invocation) bool {
	return g.escalation.EscalateOnFailure &&
		invocation.Binding.SandboxRequirement == tool.SandboxStrong
}

func policyInput(callID string, invocation Invocation) policy.Invocation {
	return policy.Invocation{
		CallID: callID, Tool: invocation.Tool, Arguments: invocation.Arguments,
		Source:    invocation.Ref.Source,
		Resources: invocation.Resources, Capability: invocation.Binding.Capability,
		Access:    invocation.Binding.AccessMode,
		Sandbox:   invocation.Binding.SandboxRequirement,
		Effect:    invocation.Binding.Effect,
		Journaled: invocation.Binding.Journaled(), Validated: true,
	}
}

func egressDeniedTarget(outcome tool.Outcome, executeErr error) (tool.NetworkTarget, bool) {
	if outcome.Security != nil && outcome.Security.EgressDenied != nil {
		target := *outcome.Security.EgressDenied
		if strings.TrimSpace(target.Host) != "" {
			return target, true
		}
	}
	if denied, ok := egress.DeniedTarget(executeErr); ok {
		return tool.NetworkTarget{
			Host: denied.Host, Protocol: denied.Protocol,
			Port: denied.Port, Method: denied.Method,
		}, true
	}
	return tool.NetworkTarget{}, false
}

func softFailEgressApproval(err error) bool {
	if err == nil {
		return false
	}
	var decision *policy.DecisionError
	if errors.As(err, &decision) {
		switch decision.Code {
		case "approval_denied", "approval_canceled", "approval_expired":
			return true
		}
	}
	return false
}

func (g *Guard) approveEgressTarget(
	ctx context.Context, invocation Invocation, callID string, target tool.NetworkTarget,
) error {
	host := strings.ToLower(strings.TrimSpace(target.Host))
	protocol := strings.ToLower(strings.TrimSpace(target.Protocol))
	if protocol == "" {
		protocol = "https"
	}
	if host == "" {
		return errors.New("egress approval requires a host")
	}
	endpoint := &url.URL{Scheme: protocol, Host: host, Path: "/"}
	if target.Port != 0 {
		endpoint.Host = net.JoinHostPort(host, strconv.Itoa(int(target.Port)))
	} else if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		endpoint.Host = "[" + host + "]"
	}
	parsed, ok := policy.ParseNetworkTarget(endpoint.String())
	if !ok || parsed.Host != host || (protocol != "http" && protocol != "https") {
		return errors.New("egress approval requires a valid HTTP target")
	}
	method := strings.ToUpper(strings.TrimSpace(target.Method))
	if strings.ContainsAny(method, " \t\r\n") {
		return errors.New("egress approval requires a valid HTTP method")
	}
	var methods []string
	if method != "" {
		methods = []string{method}
	}
	hostResource := policy.HostResource(parsed, tool.AccessRead)
	hostResource.Methods = methods
	hostResource.AllowPrivate = true
	resources := []tool.Resource{
		hostResource,
		{Kind: "url", ID: endpoint.String(), Access: tool.AccessRead, Methods: methods},
	}
	policyInvocation := policyInput(callID, invocation)
	policyInvocation.Resources = resources
	started := g.now()
	decision := g.policy.Evaluate(policyInvocation)
	reviewLatency := g.now().Sub(started)
	g.observeApproval("evaluated", policyInvocation, decision, 0)
	if decision.Action == policy.ActionDeny || decision.Action == policy.ActionHold {
		g.observeApproval("denied", policyInvocation, decision, 0)
		return &policy.DecisionError{Code: decision.Code, Reason: decision.Reason}
	}
	if decision.Action == policy.ActionAllow {
		if decision.Code == "auto_review_allowed" {
			g.observeApproval("auto_allowed", policyInvocation, decision, reviewLatency)
		}
		g.grantNetworkHosts(ctx, policyInvocation)
		return nil
	}
	now := g.now()
	if g.policy.Approvals != nil && g.policy.Approvals.MatchInvocation(policyInvocation, now) {
		g.observeApproval("grant_hit", policyInvocation, decision, 0)
		g.grantNetworkHosts(ctx, policyInvocation)
		return nil
	}
	g.observeApproval("human_required", policyInvocation, decision, reviewLatency)
	approval, err := g.waitForApproval(
		ctx, invocation, policyInvocation, now,
		approvalAsk{
			Code: ApprovalReasonNetworkHost,
			Network: &NetworkApprovalContext{
				Host: parsed.Host, Protocol: parsed.Protocol, Port: parsed.Port,
				Methods: methods, AllowPrivate: hostResource.AllowPrivate,
				Mode: string(policy.NetworkImmediate),
			},
			DisableReplace: true,
		},
	)
	if err != nil {
		return err
	}
	// This approval is consumed by the retry of the current call. A once entry
	// must not survive in the session cache for a later, identical redirect.
	if approval.Scope != policy.ApprovalOnce {
		if err := g.cacheApproval(ctx, policyInvocation, approval); err != nil {
			return err
		}
	}
	g.grantNetworkHosts(ctx, policyInvocation)
	return nil
}

func invocationWritePaths(invocation Invocation) []string {
	var paths []string
	for _, resource := range invocation.Resources {
		if resource.Kind == "file" && resource.Access == tool.AccessWrite && resource.Path != "" {
			paths = append(paths, resource.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func (g *Guard) prepareFileWrites(
	ctx context.Context, paths []string, requireRead bool,
) (map[string]workspacejournal.Fingerprint, error) {
	expected, err := g.validateFileWrites(paths, requireRead)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if g.journal == nil {
			continue
		}
		if err := g.journal.Before(ctx, path); err != nil {
			return nil, fmt.Errorf("journal before-image %q: %w", path, err)
		}
		if requireRead {
			if _, err := g.readTracker.ValidateWrite(path); err != nil {
				return nil, g.readValidationError(path, err)
			}
		}
		current, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			return nil, err
		}
		expected[path] = current
	}
	return expected, nil
}

func (g *Guard) validateFileWrites(
	paths []string,
	requireRead bool,
) (map[string]workspacejournal.Fingerprint, error) {
	expected := make(map[string]workspacejournal.Fingerprint, len(paths))
	for _, path := range paths {
		if requireRead {
			if _, err := g.readTracker.ValidateWrite(path); err != nil {
				return nil, g.readValidationError(path, err)
			}
		}
		current, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			return nil, err
		}
		if requireRead {
			if _, err := g.readTracker.ValidateWrite(path); err != nil {
				return nil, g.readValidationError(path, err)
			}
		}
		expected[path] = current
	}
	return expected, nil
}

func (g *Guard) recordExactEditProofs(
	ctx context.Context,
	executor tool.Executor,
	arguments json.RawMessage,
	writePaths []string,
) error {
	provider, ok := executor.(tool.ExactEditProofProvider)
	if !ok {
		return nil
	}
	proofs, err := provider.ExactEditProofs(ctx, arguments)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(writePaths))
	for _, path := range writePaths {
		allowed[filepath.Clean(path)] = true
	}
	for _, proof := range proofs {
		path := filepath.Clean(proof.Path)
		if !allowed[path] || proof.Digest == "" {
			return tool.Precondition(fmt.Errorf(
				"exact edit proof is not bound to a declared write path",
			))
		}
		fingerprint, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			return err
		}
		if !fingerprint.Exists || fingerprint.SHA256 != proof.Digest {
			g.readTracker.Invalidate(path)
			return g.readValidationError(path, workspacejournal.ErrStale)
		}
		if err := g.readTracker.RecordFingerprint(fingerprint); err != nil {
			return g.readValidationError(path, err)
		}
	}
	return nil
}

func (g *Guard) preflightFileWrites(invocation Invocation) error {
	for _, path := range invocationWritePaths(invocation) {
		fingerprint, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			return tool.Precondition(fmt.Errorf(
				"preflight write path %q: %w",
				path,
				err,
			))
		}
		if fingerprint.Exists || !invocation.Binding.ValidateMissingWriteParent {
			continue
		}
		parent, err := os.Stat(filepath.Dir(path))
		if err != nil {
			return tool.Precondition(fmt.Errorf(
				"preflight write path %q requires an existing parent directory: %w",
				path,
				err,
			))
		}
		if !parent.IsDir() {
			return tool.Precondition(fmt.Errorf(
				"preflight write path %q parent is not a directory",
				path,
			))
		}
	}
	return nil
}

func (g *Guard) readValidationError(path string, cause error) error {
	relative, err := filepath.Rel(g.workspace, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		relative = path
	}
	return &workspacejournal.ReadValidationError{
		Path: filepath.ToSlash(relative), Cause: cause,
	}
}

func (g *Guard) recordFileRead(
	result *tool.Result, invocation Invocation, before *workspacejournal.Fingerprint,
) error {
	for _, resource := range invocation.Resources {
		if resource.Kind != "file" || resource.Path == "" {
			continue
		}
		fingerprint, err := g.readTracker.Record(resource.Path)
		if err != nil {
			return fmt.Errorf("record file read fingerprint: %w", err)
		}
		if before == nil || !workspacejournal.Equal(*before, fingerprint) {
			g.readTracker.Invalidate(resource.Path)
			return fmt.Errorf("file read race %q: %w", resource.Path, workspacejournal.ErrStale)
		}
		tool.EnsureOutcomeFacts(result).WorkspaceRead = &tool.WorkspaceReadFact{
			Path: fingerprint.Path, Digest: fingerprint.SHA256,
		}
	}
	return nil
}

func (g *Guard) finishFileWrites(
	ctx context.Context,
	paths []string,
	expected map[string]workspacejournal.Fingerprint,
	result *tool.Result,
	succeeded, refreshRead, runDiagnostics bool,
) error {
	var receipts []diagnostics.Receipt
	var changes []FileChange
	for _, path := range paths {
		var after workspacejournal.Fingerprint
		if g.journal != nil {
			var err error
			after, err = g.journal.AfterFingerprint(path)
			if err != nil {
				return fmt.Errorf("journal commit record %q: %w", path, err)
			}
		} else {
			var err error
			after, _, _, err = workspacejournal.Snapshot(path)
			if err != nil {
				return fmt.Errorf("snapshot write result %q: %w", path, err)
			}
		}
		if !succeeded {
			g.readTracker.Invalidate(path)
			continue
		}
		if refreshRead || !expected[path].Exists {
			if err := g.readTracker.RecordFingerprint(after); err != nil {
				return fmt.Errorf("record post-write fingerprint %q: %w", path, err)
			}
		} else {
			g.readTracker.Invalidate(path)
		}
		change, changed, err := g.observeFileChange(ctx, expected[path], path)
		if err != nil {
			return fmt.Errorf("observe write %q: %w", path, err)
		}
		if changed {
			changes = append(changes, change)
		}
	}
	if !succeeded {
		return nil
	}
	if len(changes) != 0 {
		facts := tool.EnsureOutcomeFacts(result)
		facts.WorkspaceChanges = append(facts.WorkspaceChanges, changes...)
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["observed_changes"] = len(changes)
	if runDiagnostics {
		for _, path := range paths {
			receipt, err := g.diagnostics.Run(ctx, path)
			if err != nil {
				if errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("post-edit diagnostics %q: %w", path, err)
				}
				receipt = diagnostics.Receipt{
					Path: path, Status: "unavailable", Diagnostics: []diagnostics.Diagnostic{},
					Message: err.Error(), ErrorCategory: "runner_failure",
				}
			}
			receipts = append(receipts, receipt)
		}
		tool.EnsureOutcomeFacts(result).Diagnostics = append(
			[]diagnostics.Receipt(nil),
			receipts...,
		)
	}
	return nil
}

func (g *Guard) finishBrokerFileWrites(
	ctx context.Context,
	paths []string,
	result *tool.Result,
	succeeded bool,
) error {
	for _, path := range paths {
		if !succeeded {
			g.readTracker.Invalidate(path)
			continue
		}
		after, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			return fmt.Errorf("snapshot broker write %q: %w", path, err)
		}
		if err := g.readTracker.RecordFingerprint(after); err != nil {
			return fmt.Errorf("record broker write fingerprint %q: %w", path, err)
		}
		receipt, err := g.diagnostics.Run(ctx, path)
		if err != nil {
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("post-edit diagnostics %q: %w", path, err)
			}
			receipt = diagnostics.Receipt{
				Path: path, Status: "unavailable",
				Diagnostics: []diagnostics.Diagnostic{},
				Message:     err.Error(), ErrorCategory: "runner_failure",
			}
		}
		tool.EnsureOutcomeFacts(result).Diagnostics = append(
			tool.EnsureOutcomeFacts(result).Diagnostics,
			receipt,
		)
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["observed_changes"] = len(
		tool.EnsureOutcomeFacts(result).WorkspaceChanges,
	)
	return nil
}

func (g *Guard) observeFileChange(
	ctx context.Context, before workspacejournal.Fingerprint, path string,
) (FileChange, bool, error) {
	after, _, _, err := workspacejournal.Snapshot(path)
	if err != nil {
		return FileChange{}, false, err
	}
	kind := workspacejournal.Record{Before: before, After: after}.Kind()
	if kind == "" {
		return FileChange{}, false, nil
	}
	relative, err := g.workspaceRelative(path)
	if err != nil {
		return FileChange{}, false, err
	}
	change := FileChange{Path: relative, Kind: kind}
	if after.Exists {
		change.AfterDigest = after.SHA256
	} else {
		change.AfterDigest = "missing"
	}
	stats, counted, err := g.countLines(ctx, path)
	if err != nil {
		return FileChange{}, false, err
	}
	if counted {
		change.Added, change.Removed = stats.Added, stats.Removed
	}
	return change, true, nil
}

func (g *Guard) countLines(
	ctx context.Context, path string,
) (textdiff.Stats, bool, error) {
	if g.journal == nil {
		return textdiff.Stats{}, false, nil
	}
	beforeData, existed, found, err := g.journal.BeforeImage(ctx, path)
	if err != nil || !found {
		return textdiff.Stats{}, false, err
	}
	afterData, err := os.ReadFile(path)
	afterMissing := false
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		afterMissing = true
	default:
		return textdiff.Stats{}, false, err
	}
	stats, err := textdiff.Count(
		textdiff.Content{Data: beforeData, Missing: !existed},
		textdiff.Content{Data: afterData, Missing: afterMissing},
	)
	if errors.Is(err, textdiff.ErrBinary) {
		return textdiff.Stats{}, false, nil
	}
	if err != nil {
		return textdiff.Stats{}, false, err
	}
	return stats, true, nil
}

func (g *Guard) prepare(
	ctx context.Context, name, callID string, raw json.RawMessage, binding tool.CatalogBinding,
) (Invocation, tool.Executor, error) {
	ref, descriptor, executor, err := g.registry.ResolveBoundRef(name, binding)
	if err != nil {
		return Invocation{}, nil, err
	}
	trusted, err := g.registry.ResolveTrustedBinding(ref)
	if err != nil {
		return Invocation{}, nil, err
	}
	descriptor = tool.ApplyTrustedBinding(descriptor, trusted)
	canonical := ref.Name
	repaired := tool.RepairArguments(raw)
	arguments, err := tool.NormalizeArguments(descriptor.InputSchema, repaired)
	if err != nil {
		return Invocation{}, nil, fmt.Errorf("tool %q arguments: %w", canonical, err)
	}
	if expander, ok := executor.(tool.ArgumentExpander); ok {
		arguments, err = expander.ExpandArguments(ctx, arguments)
		if err != nil {
			return Invocation{}, nil, fmt.Errorf(
				"%w: tool %q expand: %v",
				tool.ErrInvalidArguments,
				canonical,
				err,
			)
		}
	}
	arguments, err = g.rewriteAbsolutePathArgs(descriptor, arguments)
	if err != nil {
		return Invocation{}, nil, fmt.Errorf("tool %q resources: %w", canonical, err)
	}
	resources, err := g.resolveResources(canonical, descriptor, arguments)
	if err != nil {
		return Invocation{}, nil, fmt.Errorf("tool %q resources: %w", canonical, err)
	}
	disposition := tool.DispositionFor(executor)
	if disposition == tool.DispositionAbortImmediately &&
		(descriptor.Capability != tool.CapabilityRead ||
			hasConsequentialWrite(resources)) {
		return Invocation{}, nil, fmt.Errorf(
			"tool %q cannot abort immediately with consequential effects",
			canonical,
		)
	}
	if descriptor.ParallelPolicy == tool.ParallelSerial {
		resources = append(resources, tool.Resource{
			Kind: "parallel", ID: "serial-tools", Access: tool.AccessWrite, Tree: true,
		})
	}
	if descriptor.SandboxRequirement == tool.SandboxStrong {
		if err := sandbox.RequireControls(
			g.registry.InjectedSandbox(canonical),
			trusted.Required,
		); err != nil {
			return Invocation{}, nil, err
		}
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = callID
	return Invocation{
		Identity: identity, CallID: callID, Tool: canonical, Ref: ref,
		Arguments: arguments, Resources: resources, Descriptor: descriptor,
		Binding: trusted,
		Source:  tool.InvocationSourceFrom(ctx), Disposition: disposition,
	}, executor, nil
}

func hasConsequentialWrite(resources []tool.Resource) bool {
	for _, resource := range resources {
		if resource.Access == tool.AccessWrite {
			return true
		}
	}
	return false
}

func (g *Guard) rewriteAbsolutePathArgs(
	descriptor tool.Descriptor, arguments json.RawMessage,
) (json.RawMessage, error) {
	var values map[string]any
	if err := json.Unmarshal(arguments, &values); err != nil {
		return nil, err
	}
	changed := false
	for _, template := range descriptor.ResourceResolver.Templates {
		if template.Field == "" || !isPathKind(template.Kind) {
			continue
		}
		raw, ok := values[template.Field].(string)
		if !ok || raw == "" || !filepath.IsAbs(raw) {
			continue
		}
		rel, err := g.workspaceRelative(raw)
		if err != nil {
			return nil, err
		}
		values[template.Field] = rel
		changed = true
	}
	for _, field := range []string{
		descriptor.ResourceResolver.PathsField,
		descriptor.ResourceResolver.ReadPathsField,
	} {
		if field == "" {
			continue
		}
		rawPaths, exists := values[field].([]any)
		if !exists {
			continue
		}
		for index, raw := range rawPaths {
			path, ok := raw.(string)
			if !ok || path == "" || !filepath.IsAbs(path) {
				continue
			}
			rel, err := g.workspaceRelative(path)
			if err != nil {
				return nil, err
			}
			rawPaths[index] = rel
			changed = true
		}
		values[field] = rawPaths
	}
	if !changed {
		return arguments, nil
	}
	out, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *Guard) workspaceRelative(value string) (string, error) {
	absWorkspace, err := filepath.Abs(g.workspace)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absWorkspace); err == nil {
		absWorkspace = resolved
	}
	absValue, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absValue); err == nil {
		absValue = resolved
	} else {
		resolvedParent, missing, joinErr := resolveExistingParent(absValue)
		if joinErr != nil {
			return "", errors.New("absolute resource path is not allowed")
		}
		absValue = resolvedParent
		for _, part := range missing {
			absValue = filepath.Join(absValue, part)
		}
	}
	rel, err := filepath.Rel(absWorkspace, absValue)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("absolute resource path is not allowed")
	}
	if rel == "." {
		return ".", nil
	}
	return rel, nil
}

func resolveExistingParent(path string) (string, []string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return resolved, missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, err
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

func (g *Guard) waitForApproval(
	ctx context.Context,
	invocation Invocation,
	policyInvocation policy.Invocation,
	now time.Time,
	ask ...approvalAsk,
) (ApprovalDecision, error) {
	handler := g.approvalHandler()
	if handler == nil {
		return ApprovalDecision{}, &policy.DecisionError{
			Code: "approval_host_unavailable", Reason: "approval host is not connected",
		}
	}
	var opts approvalAsk
	if len(ask) > 0 {
		opts = ask[0]
	}
	var expiresAt time.Time
	if g.approvalTTL > 0 {
		expiresAt = now.Add(g.approvalTTL)
	}
	g.mu.Lock()
	recovered, recovering := g.recovered[invocation.CallID]
	if recovering {
		delete(g.recovered, invocation.CallID)
		expiresAt = recovered.ExpiresAt
	}
	g.mu.Unlock()
	request, err := policy.NewApprovalRequestForScope(
		policyInvocation, policy.ApprovalOnce, expiresAt,
	)
	if err != nil {
		return ApprovalDecision{}, err
	}
	requestID := randomID("approval_")
	if recovering {
		requestID = recovered.RequestID
	}
	fields := schemaProperties(invocation.Descriptor.InputSchema)
	scopes := []policy.ApprovalScope{policy.ApprovalOnce}
	if request.Grant != nil {
		scopes = append(scopes, policy.ApprovalSession)
		if g.persistAllow != nil {
			scopes = append(scopes, policy.ApprovalAlways)
		}
	}
	if len(opts.AllowedScopes) != 0 {
		allowed := make(map[policy.ApprovalScope]bool, len(opts.AllowedScopes))
		for _, scope := range opts.AllowedScopes {
			allowed[scope] = true
		}
		scopes = slices.DeleteFunc(scopes, func(scope policy.ApprovalScope) bool {
			return !allowed[scope]
		})
	}
	replacementAllowed := !opts.DisableReplace
	modifiable := fields
	if opts.DisableReplace {
		modifiable = nil
	}
	event := ApprovalRequest{
		RequestID: requestID, CallID: invocation.CallID, Tool: invocation.Tool,
		Arguments: request.Arguments, ArgumentsDigest: request.ArgumentsDigest,
		Resources: request.Resources, AllowedScopes: scopes,
		ExpiresAt: expiresAt, ReplacementAllowed: replacementAllowed,
		ModifiableArguments: modifiable, ReasonCode: opts.Code,
		Network: opts.Network, EditPlan: opts.EditPlan, Grant: request.Grant,
		AdditionalPermission: opts.AdditionalPermission,
	}
	effect := policy.NormalizeEffect(policyInvocation)
	event.Effect, event.Risk = effect.Kind, effect.Risk
	if recovering {
		event = recovered
	}
	if opts.AdditionalPermission != nil {
		event.Effect = policy.EffectExternalMutation
		event.Risk = policy.RiskCritical
	}
	entry := &pending{
		callID: invocation.CallID, decision: make(chan ApprovalDecision, 1),
		resume: make(chan struct{}),
	}
	g.mu.Lock()
	g.pending[requestID] = entry
	g.mu.Unlock()
	defer g.finishPending(requestID)
	if recovering {
		restore := g.approvalRecoveryHandler()
		if restore == nil {
			return ApprovalDecision{}, errors.New(
				"approval recovery handler is unavailable",
			)
		}
		if err := restore(event); err != nil {
			return ApprovalDecision{}, fmt.Errorf(
				"restore approval wait: %w", err,
			)
		}
	}
	parked := g.now()
	reportWait := func(outcome ApprovalWaitOutcome) ApprovalWait {
		waited := g.now().Sub(parked)
		wait := ApprovalWait{
			RequestID: requestID, CallID: invocation.CallID, Tool: invocation.Tool,
			Waited: waited, Outcome: outcome,
		}
		observe := g.approvalWaitObserver()
		if observe != nil {
			observe(wait)
		}
		g.observeApproval(
			"waited", policyInvocation, policy.Decision{Code: event.ReasonCode}, waited,
		)
		return wait
	}
	if !recovering {
		if err := handler(ctx, event); err != nil {
			return ApprovalDecision{}, fmt.Errorf("emit approval request: %w", err)
		}
	}
	var expiry <-chan time.Time
	var timer *time.Timer
	if !expiresAt.IsZero() {
		timer = time.NewTimer(max(time.Millisecond, expiresAt.Sub(g.now())))
		expiry = timer.C
		defer timer.Stop()
	}
	select {
	case <-ctx.Done():
		reportWait(ApprovalWaitCanceled)
		return ApprovalDecision{}, ctx.Err()
	case <-expiry:
		wait := reportWait(ApprovalWaitExpired)
		if expire := g.approvalExpiryHandler(); expire != nil {
			if err := expire(wait); err != nil {
				return ApprovalDecision{}, fmt.Errorf(
					"expire approval wait: %w", err,
				)
			}
		}
		return ApprovalDecision{}, &policy.DecisionError{
			Code: "approval_expired", Reason: "approval request expired",
		}
	case decision := <-entry.decision:
		select {
		case <-ctx.Done():
			reportWait(ApprovalWaitCanceled)
			return ApprovalDecision{}, ctx.Err()
		case <-entry.resume:
			reportWait(ApprovalWaitDecided)
		}
		if decision.Canceled {
			return ApprovalDecision{}, &policy.DecisionError{
				Code: "approval_canceled", Reason: "approval was canceled",
			}
		}
		if !decision.Approved {
			return ApprovalDecision{}, &policy.DecisionError{
				Code: "approval_denied", Reason: "approval was denied",
			}
		}
		if decision.Scope == "" {
			decision.Scope = policy.ApprovalOnce
		}
		if !slices.Contains(scopes, decision.Scope) {
			return ApprovalDecision{}, &policy.DecisionError{
				Code: "approval_scope_denied", Reason: "approval scope is not allowed",
			}
		}
		if decision.ExpiresAt.IsZero() {
			decision.ExpiresAt = expiresAt
		}
		if !decision.ExpiresAt.IsZero() {
			if !decision.ExpiresAt.After(g.now()) ||
				(!expiresAt.IsZero() && decision.ExpiresAt.After(expiresAt)) {
				return ApprovalDecision{}, &policy.DecisionError{
					Code: "approval_expired", Reason: "approval expiry is invalid",
				}
			}
		}
		return decision, nil
	}
}

func (g *Guard) observeApproval(
	outcome string,
	invocation policy.Invocation,
	decision policy.Decision,
	latency time.Duration,
) {
	if g.observe != nil {
		effect := policy.NormalizeEffect(invocation)
		g.observe(
			outcome, string(effect.Kind), string(effect.Risk), decision.Code, latency,
		)
	}
}

func (g *Guard) RestoreApproval(request ApprovalRequest) error {
	if request.RequestID == "" || request.CallID == "" {
		return errors.New("restored approval request is incomplete")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, pending := g.pending[request.RequestID]; pending {
		return errors.New("restored approval request is already pending")
	}
	g.recovered[request.CallID] = request
	return nil
}

func (g *Guard) cacheApproval(
	ctx context.Context, invocation policy.Invocation, decision ApprovalDecision,
) error {
	if decision.Scope == policy.ApprovalAlways {
		if g.persistAllow != nil {
			if err := g.persistAllow(invocation); err != nil {
				return fmt.Errorf("persist always allow: %w", err)
			}
		}
		decision.Scope = policy.ApprovalSession
	}
	request, err := policy.NewApprovalRequestForScope(
		invocation, decision.Scope, decision.ExpiresAt,
	)
	if err != nil {
		return err
	}
	if g.policy.Approvals == nil {
		g.policy.Approvals = policy.NewApprovalCache()
	}
	if err := g.policy.Approvals.Add(request, decision.Scope); err != nil {
		return err
	}
	g.grantNetworkHosts(ctx, invocation)
	return nil
}

func networkApprovalAsk(
	invocation policy.Invocation, capability tool.Capability,
) approvalAsk {
	if capability != tool.CapabilityNetwork && capability != tool.CapabilityProcess {
		return approvalAsk{}
	}
	var network *NetworkApprovalContext
	for _, resource := range invocation.Resources {
		if resource.Kind == "url" && strings.TrimSpace(resource.ID) != "" {
			if target, ok := policy.ParseNetworkTarget(resource.ID); ok {
				network = &NetworkApprovalContext{
					Host: target.Host, Protocol: target.Protocol, Port: target.Port,
				}
				break
			}
		}
	}
	if network == nil {
		for _, resource := range invocation.Resources {
			if resource.Kind == "host" && strings.TrimSpace(resource.ID) != "" {
				network = &NetworkApprovalContext{
					Host: resource.ID, Protocol: resource.Protocol, Port: resource.Port,
					Methods:      append([]string(nil), resource.Methods...),
					AllowPrivate: resource.AllowPrivate,
				}
				break
			}
		}
	}
	if network == nil {
		return approvalAsk{}
	}
	network.Mode = string(policy.NetworkImmediate)
	return approvalAsk{
		Code: ApprovalReasonNetworkHost, Network: network,
	}
}

func (g *Guard) grantNetworkHosts(ctx context.Context, invocation policy.Invocation) {
	if g == nil {
		return
	}
	allow := func(target egress.Target) {
		if invocation.Capability == tool.CapabilityNetwork {
			egress.AllowInScope(ctx, target)
		} else if g.onNetworkAllow != nil {
			g.onNetworkAllow(invocation.Capability, target)
		}
	}
	for _, resource := range invocation.Resources {
		switch resource.Kind {
		case "host":
			if resource.Protocol == "loopback" {
				continue
			}
			allow(egress.Target{
				Host: resource.ID, Protocol: resource.Protocol, Port: resource.Port,
				Methods: resource.Methods, AllowPrivate: resource.AllowPrivate,
			})
		case "url":
			if target, ok := policy.ParseNetworkTarget(resource.ID); ok {
				allow(egress.Target{
					Host: target.Host, Protocol: target.Protocol, Port: target.Port,
					Methods:      resource.Methods,
					AllowPrivate: true,
				})
			}
		}
	}
}

func (g *Guard) Decide(decision ApprovalDecision) error {
	if err := g.StageDecision(decision); err != nil {
		return err
	}
	return g.Resume(decision.RequestID)
}

func (g *Guard) StageDecision(decision ApprovalDecision) error {
	if decision.RequestID == "" {
		return errors.New("approval request id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.pending[decision.RequestID]
	if entry == nil {
		if _, exists := g.completed[decision.RequestID]; exists {
			return errors.New("approval decision is duplicate or late")
		}
		return errors.New("approval request is unknown")
	}
	if decision.CallID != "" && decision.CallID != entry.callID {
		return errors.New("approval decision call id does not match request")
	}
	if entry.decided {
		return errors.New("approval decision is duplicate")
	}
	entry.decided = true
	entry.decision <- decision
	return nil
}

func (g *Guard) Resume(requestID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.pending[requestID]
	if entry == nil {
		return errors.New("approval request is duplicate or late")
	}
	if !entry.decided {
		return errors.New("approval request has no decision")
	}
	close(entry.resume)
	delete(g.pending, requestID)
	g.completed[requestID] = g.now()
	g.pruneCompletedLocked()
	return nil
}

func (g *Guard) finishPending(requestID string) {
	g.mu.Lock()
	if _, exists := g.pending[requestID]; exists {
		delete(g.pending, requestID)
		g.completed[requestID] = g.now()
	}
	g.pruneCompletedLocked()
	g.mu.Unlock()
}

// pruneCompletedLocked removes completed entries older than the approval TTL.
// Once the TTL has passed, no one can submit a decision for that request, so
// the entry is safe to remove. The caller must hold g.mu.
func (g *Guard) pruneCompletedLocked() {
	cutoff := g.now().Add(-g.approvalTTL)
	for id, completedAt := range g.completed {
		if completedAt.Before(cutoff) {
			delete(g.completed, id)
		}
	}
	if len(g.completed) > 1000 {
		// Aggressive fallback: if the map is still large, remove the oldest
		// half. This handles cases where the TTL is very long or now() is
		// not monotonic.
		type entry struct {
			id string
			at time.Time
		}
		entries := make([]entry, 0, len(g.completed))
		for id, at := range g.completed {
			entries = append(entries, entry{id, at})
		}
		// Sort oldest first and remove the oldest half.
		sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
		for i := 0; i < len(entries)/2; i++ {
			delete(g.completed, entries[i].id)
		}
	}
}

func (g *Guard) Pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

func (g *Guard) SetApprovalHandler(handler func(context.Context, ApprovalRequest) error) {
	g.mu.Lock()
	g.approvals = handler
	g.mu.Unlock()
}
func (g *Guard) SetApprovalRecoveryHandler(handler func(ApprovalRequest) error) {
	g.mu.Lock()
	g.restoreWait = handler
	g.mu.Unlock()
}
func (g *Guard) SetApprovalWaitObserver(observe func(ApprovalWait)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.approvalWait = observe
	g.mu.Unlock()
}
func (g *Guard) SetApprovalExpiryHandler(expire func(ApprovalWait) error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.expireWait = expire
	g.mu.Unlock()
}
func (g *Guard) SwapPolicy(next *policy.Runtime) *policy.Runtime {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.policy
	if next != nil {
		g.policy = next
	}
	return prev
}
func (g *Guard) Policy() *policy.Runtime {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.policy
}

func (g *Guard) approvalHandler() func(context.Context, ApprovalRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.approvals
}

func (g *Guard) approvalRecoveryHandler() func(ApprovalRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restoreWait
}

func (g *Guard) approvalWaitObserver() func(ApprovalWait) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.approvalWait
}

func (g *Guard) approvalExpiryHandler() func(ApprovalWait) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.expireWait
}

func (g *Guard) resolveResources(
	toolName string,
	descriptor tool.Descriptor,
	arguments json.RawMessage,
) ([]tool.Resource, error) {
	var values map[string]any
	if err := json.Unmarshal(arguments, &values); err != nil {
		return nil, err
	}
	var resources []tool.Resource
	for _, template := range descriptor.ResourceResolver.Templates {
		value := template.ID
		if template.Field != "" {
			field, exists := values[template.Field]
			if !exists {
				continue
			}
			text, ok := field.(string)
			if !ok {
				return nil, fmt.Errorf("resource field %q is not a string", template.Field)
			}
			value = text
		}
		if value == "" && !isPathKind(template.Kind) {
			continue
		}
		resource := tool.Resource{
			Kind: template.Kind, Access: template.Access, Tree: template.Tree,
		}
		if isPathKind(template.Kind) {
			path, err := g.canonicalPath(value, template.Glob)
			if err != nil {
				return nil, err
			}
			resource.Path = path
		} else {
			resource.ID = strings.TrimSpace(value)
		}
		resources = append(resources, resource)
	}
	for _, resource := range append([]tool.Resource(nil), resources...) {
		if resource.Kind != "url" || resource.ID == "" {
			continue
		}
		target, ok := policy.ParseNetworkTarget(resource.ID)
		if !ok {
			continue
		}
		resources = append(resources, policy.HostResource(target, resource.Access))
	}
	if field := descriptor.ResourceResolver.PatchField; field != "" {
		patch, _ := values[field].(string)
		for _, path := range patchPaths(patch) {
			canonical, err := g.canonicalPath(path, false)
			if err != nil {
				return nil, err
			}
			resources = append(resources, tool.Resource{
				Kind: "file", Path: canonical, Access: tool.AccessWrite,
			})
		}
	}
	if field := descriptor.ResourceResolver.PathsField; field != "" {
		paths, err := exactPaths(values[field], "write")
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			canonical, err := g.canonicalPath(path, false)
			if err != nil {
				return nil, err
			}
			resources = append(resources, tool.Resource{
				Kind: "file", Path: canonical, Access: tool.AccessWrite,
			})
		}
	}
	if field := descriptor.ResourceResolver.ReadPathsField; field != "" {
		paths, err := exactPaths(values[field], "read")
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			canonical, err := g.canonicalPath(path, false)
			if err != nil {
				return nil, err
			}
			resources = append(resources, tool.Resource{
				Kind: "file", Path: canonical, Access: tool.AccessRead,
			})
		}
	}
	if field := descriptor.ResourceResolver.TrustedHostPathField; field != "" {
		value, ok := values[field].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, errors.New("trusted host path must be a non-empty string")
		}
		backend := g.registry.InjectedSandbox(toolName)
		sandboxPolicy, ok := sandbox.BackendPolicy(backend)
		if !ok {
			return nil, errors.New("trusted host path requires an injected sandbox policy")
		}
		resolver, err := sandbox.NewTrustedHostPathResolver(
			sandboxPolicy.WorkspaceRoot,
			sandboxPolicy.PrivateTemp,
		)
		if err != nil {
			return nil, err
		}
		path, err := resolver.Resolve(value, sandbox.AllowMissing)
		if err != nil {
			return nil, err
		}
		resources = append(resources, tool.Resource{
			Kind: "file", Path: path, Access: tool.AccessRead,
		})
	}
	if field := descriptor.ResourceResolver.NetworkTargetsField; field != "" {
		targets, err := networkTargets(values[field])
		if err != nil {
			return nil, err
		}
		resources = append(resources, targets...)
	}
	if field := descriptor.ResourceResolver.LoopbackField; field != "" {
		enabled, ok := values[field].(bool)
		if values[field] != nil && !ok {
			return nil, errors.New("allow_loopback must be a boolean")
		}
		if enabled {
			resources = append(resources, tool.Resource{
				Kind: "host", ID: "localhost", Access: tool.AccessWrite,
				Protocol: "loopback", Methods: []string{"BIND", "CONNECT"},
				AllowPrivate: true,
			})
		}
	}
	if field := descriptor.ResourceResolver.ChangesField; field != "" {
		paths, err := changePaths(values[field])
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			canonical, err := g.canonicalPath(path, false)
			if err != nil {
				return nil, err
			}
			resources = append(resources, tool.Resource{
				Kind: "file", Path: canonical, Access: tool.AccessWrite,
			})
		}
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Key() < resources[j].Key() })
	result := resources[:0]
	for _, resource := range resources {
		if len(result) == 0 || result[len(result)-1].Key() != resource.Key() {
			result = append(result, resource)
		}
	}
	return result, nil
}

func exactPaths(value any, access string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("exact %s paths must be an array", access)
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		path, ok := item.(string)
		if !ok || strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("exact %s path must be a non-empty string", access)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func networkTargets(value any) ([]tool.Resource, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("network targets must be an array")
	}
	if len(items) > 32 {
		return nil, errors.New("network targets exceed the 32-target limit")
	}
	resources := make([]tool.Resource, 0, len(items))
	for _, item := range items {
		target, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("network target must be an object")
		}
		host, _ := target["host"].(string)
		protocol, _ := target["protocol"].(string)
		portValue, _ := target["port"].(float64)
		allowPrivate, _ := target["allow_private"].(bool)
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		if host == "" || strings.ContainsAny(host, "/\\@") ||
			(protocol != "http" && protocol != "https") ||
			portValue < 1 || portValue > 65535 || portValue != float64(uint16(portValue)) {
			return nil, errors.New("network target requires host, protocol, and port")
		}
		methods, err := networkMethods(target["methods"])
		if err != nil {
			return nil, err
		}
		resources = append(resources, tool.Resource{
			Kind: "host", ID: host, Access: tool.AccessWrite,
			Protocol: protocol, Port: uint16(portValue),
			Methods: methods, AllowPrivate: allowPrivate,
		})
	}
	return resources, nil
}

func networkMethods(value any) ([]string, error) {
	items, ok := value.([]any)
	if value == nil {
		return nil, nil
	}
	if !ok || len(items) > 16 {
		return nil, errors.New("network target methods must be a bounded array")
	}
	methods := make([]string, 0, len(items))
	for _, item := range items {
		method, ok := item.(string)
		method = strings.ToUpper(strings.TrimSpace(method))
		if !ok || method == "" || strings.ContainsAny(method, " \t\r\n") {
			return nil, errors.New("network target method is invalid")
		}
		methods = append(methods, method)
	}
	slices.Sort(methods)
	return slices.Compact(methods), nil
}

func (g *Guard) canonicalPath(value string, glob bool) (string, error) {
	if value == "" {
		value = "."
	}
	if filepath.IsAbs(value) {
		rel, err := g.workspaceRelative(value)
		if err != nil {
			return "", err
		}
		value = rel
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("resource path escapes workspace")
	}
	suffix := ""
	base := clean
	if glob {
		if index := strings.IndexAny(base, "*?[{"); index >= 0 {
			slash := strings.LastIndex(base[:index], string(filepath.Separator))
			if slash < 0 {
				base, suffix = ".", base
			} else {
				base, suffix = base[:slash], base[slash+1:]
			}
		}
	}
	candidate := filepath.Join(g.workspace, base)
	canonical, err := canonicalMissing(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(g.workspace, canonical)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("resource path resolves outside workspace")
	}
	if suffix != "" {
		canonical = filepath.Join(canonical, suffix)
	}
	return filepath.Clean(canonical), nil
}

func canonicalMissing(path string) (string, error) {
	current := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func patchPaths(patch string) []string {
	var result []string
	for line := range strings.SplitSeq(patch, "\n") {
		for _, marker := range []string{"--- ", "+++ ", "rename from ", "rename to "} {
			value, found := strings.CutPrefix(line, marker)
			if !found {
				continue
			}
			fields := strings.Fields(strings.TrimSpace(value))
			if len(fields) == 0 || fields[0] == "/dev/null" {
				break
			}
			path := strings.TrimPrefix(strings.TrimPrefix(fields[0], "a/"), "b/")
			if path != "" {
				result = append(result, path)
			}
			break
		}
	}
	return result
}

func changePaths(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("changes argument is not an array")
	}
	var paths []string
	for index, item := range items {
		change, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("change %d is not an object", index)
		}
		for _, field := range []string{"path", "to"} {
			raw, exists := change[field]
			if !exists {
				continue
			}
			text, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("change %d field %q is not a string", index, field)
			}
			if text = strings.TrimSpace(text); text != "" {
				paths = append(paths, text)
			}
		}
	}
	return paths, nil
}

func schemaProperties(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	result := make([]string, 0, len(properties))
	for name := range properties {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func isPathKind(kind string) bool {
	return kind == "file" || kind == "directory" || kind == "repo" || kind == "workspace"
}

func randomID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(value[:])
}

func canonicalizeRuleResource(rule *policy.Rule, workspace string) error {
	rule.ResourcePath = ""
	// A rule may apply to both path and ID resources. Preserve its literal
	// spelling and resolve only the filesystem interpretation separately.
	if rule.Resource == "" || rule.Resource == "*" || !isPathRule(rule.Resource) {
		return nil
	}
	if filepath.IsAbs(rule.Resource) {
		canonical, err := canonicalMissing(rule.Resource)
		if err != nil {
			return err
		}
		rule.ResourcePath = canonical
		return nil
	}
	clean := filepath.Clean(rule.Resource)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("rule resource escapes workspace")
	}
	canonical, err := canonicalMissing(filepath.Join(workspace, clean))
	if err != nil {
		return err
	}
	rule.ResourcePath = canonical
	return nil
}

func isPathRule(value string) bool {
	if parsed, err := url.Parse(value); err == nil && parsed.IsAbs() && parsed.Host != "" {
		return false
	}
	return strings.Contains(value, "/") || strings.Contains(value, `\`) ||
		value == "." || !strings.Contains(value, ":")
}
