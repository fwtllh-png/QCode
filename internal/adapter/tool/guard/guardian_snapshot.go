package guard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ReviewContentRequest supplies optional extra evidence and known gaps, never
// tool or model JSON. Guard discovers script dependencies itself; callers cannot
// declare coverage complete. MaxBytes is an explicit total admission budget.
type ReviewContentRequest struct {
	AttemptID string
	Paths     []string
	Missing   []string
	MaxBytes  int64
}

// ReviewExecution holds one command's reviewed workspace until execution or
// cancellation. It neither calls a model nor changes a Policy decision.
type ReviewExecution struct {
	attemptID      string
	parentPolicyID string
	mu             sync.Mutex
	guard          *Guard
	provider       tool.ReviewPlanProvider
	invocation     Invocation
	identity       tool.InvocationIdentity
	plan           tool.ReviewPlan
	session        tool.IsolatedWorkspace
	backend        sandbox.Backend
	content        *filebroker.ReviewContent
	snapshot       guardian.ExecutionSnapshot
	revision       uint64
	permission     policy.Permission
	used, closed   bool
}

func (g *Guard) PrepareGuardianExecution(ctx context.Context, callID, name string, raw json.RawMessage, binding tool.CatalogBinding, request ReviewContentRequest) (_ *ReviewExecution, resultErr error) {
	if g == nil || g.isolator == nil || request.AttemptID == "" || request.MaxBytes <= 0 {
		return nil, errors.New("Guardian requires isolation, an attempt identity and a positive content budget")
	}
	invocation, executor, err := g.prepare(ctx, name, callID, append(json.RawMessage(nil), raw...), binding)
	if err != nil {
		return nil, err
	}
	runtime, err := g.samplePolicy()
	if err != nil {
		return nil, err
	}
	invocation = bindProcessAccess(invocation, runtime)
	provider, ok := executor.(tool.ReviewPlanProvider)
	if !ok || !invocation.Binding.GuardianReview || tool.CatalogSourceKind(name, invocation.Ref.Source) != securitymodel.SourceBuiltin {
		return nil, errors.New("binding is not registered for Guardian preparation")
	}
	facets := invocation.Assessment.Facets()
	if invocation.Assessment.Rule() != securitymodel.RuleProcessMutating || !facets.StrongSandbox || facets.Network || facets.LoopbackReach || facets.HostLocalTarget || facets.HostExecution || facets.FullAccess {
		return nil, errors.New("invocation is outside Guardian's bounded sandbox scope")
	}
	plan, err := provider.GuardianReviewPlan(invocation)
	if err != nil {
		return nil, err
	}
	parentPolicy, hasPolicy := sandbox.BackendPolicy(plan.Backend)
	if !hasPolicy {
		return nil, errors.New("Guardian parent sandbox policy is missing")
	}
	if plan.SourceRoot != g.workspace || len(plan.WritePaths) == 0 {
		return nil, errors.New("Guardian preparation requires the current workspace and bounded write scopes")
	}
	for _, path := range plan.WritePaths {
		if err := g.controlPlane.CheckWrite(path, true); err != nil {
			return nil, err
		}
		if pathpolicy.IsCredentialFileName(filepath.Base(path)) || pathpolicy.InCredentialLocation(path, "") {
			return nil, errors.New("Guardian preparation excludes credential write scopes")
		}
	}
	// Reserve a fresh private copy even if a caller repeats Call/Attempt IDs.
	// The existing Isolator remains responsible for copy, baseline and cleanup.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := callID + ":" + request.AttemptID + ":" + hex.EncodeToString(nonce[:])
	var session tool.IsolatedWorkspace
	switch plan.Settlement {
	case "apply":
		session, err = g.isolator.Begin(ctx, id, plan.WritePaths)
	case "discard":
		session, err = g.isolator.BeginShadow(ctx, id, plan.WritePaths)
	default:
		return nil, errors.New("Guardian settlement is invalid")
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, session.Close())
		}
	}()
	if session.Root() == plan.SourceRoot {
		return nil, errors.New("Guardian requires a private execution copy")
	}
	workspace, err := sandbox.NewWorkspace(session.Root())
	if err != nil {
		return nil, err
	}
	relativeCWD, err := filepath.Rel(plan.SourceRoot, plan.WorkingDir)
	if err != nil {
		return nil, err
	}
	backend, _, err := session.PrepareBackend(plan.Backend)
	if err != nil {
		return nil, err
	}
	backendPolicy, ok := sandbox.BackendPolicy(backend)
	if !ok || !backend.Capability().Available {
		return nil, errors.New("Guardian snapshot sandbox is unavailable")
	}
	if err := sandbox.RequireControls(backend, invocation.Binding.Required); err != nil {
		return nil, err
	}
	content, coverage, err := captureGuardianContent(ctx, workspace, relativeCWD, plan.Command, request, func(path string) error {
		arguments, _ := json.Marshal(map[string]string{"path": path})
		read, _, err := g.prepare(ctx, "file_read", callID+":review-read", arguments, tool.CatalogBinding{})
		if err != nil {
			return err
		}
		if decision := runtime.Decide(g.policyInput(read.CallID, read)); decision.Action != policy.ActionAllow {
			return errors.New("Guardian evidence read requires additional authority")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := content.ValidateReadOnly(ctx, plan.WritePaths); err != nil {
		coverage.Missing = append(coverage.Missing, err.Error())
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = callID
	snapshot := guardian.ExecutionSnapshot{
		ID: id, Root: workspace.Root(), RootIdentity: content.RootIdentity(),
		WorkingDir: filepath.Join(workspace.Root(), relativeCWD), WorkingDirIdentity: content.WorkingDirIdentity(),
		Command: plan.Command, EnvironmentDigest: plan.EnvironmentDigest,
		ResourcesDigest: reviewDigest(invocation.Assessment.Resources()), AssessmentID: invocation.Assessment.Digest(),
		SandboxPolicyID: backendPolicy.ID, Settlement: plan.Settlement, Controls: invocation.Binding.Required,
		Content: content.Entries(), CoverageComplete: len(coverage.Missing) == 0, Missing: coverage.Missing,
		WritePaths: slices.Clone(plan.WritePaths),
	}
	return &ReviewExecution{attemptID: request.AttemptID, parentPolicyID: parentPolicy.ID, guard: g, provider: provider, invocation: invocation, identity: identity, plan: plan, session: session, backend: backend, content: content, snapshot: snapshot, revision: runtime.Revision, permission: runtime.Permission}, nil
}

func captureGuardianContent(ctx context.Context, workspace *sandbox.Workspace, cwd, command string, request ReviewContentRequest, authorize func(string) error) (*filebroker.ReviewContent, guardian.ContentCoverage, error) {
	paths := slices.Clone(request.Paths)
	bodies := make(map[string][]byte)
	var content *filebroker.ReviewContent
	for {
		if err := ctx.Err(); err != nil {
			return nil, guardian.ContentCoverage{}, err
		}
		coverage := guardian.AnalyzeContent(command, cwd, bodies)
		paths = append(paths, coverage.Required...)
		slices.Sort(paths)
		paths = slices.Compact(paths)
		if content != nil && len(paths) == len(bodies) {
			coverage.Missing = append(coverage.Missing, request.Missing...)
			slices.Sort(coverage.Missing)
			coverage.Missing = slices.Compact(coverage.Missing)
			return content, coverage, nil
		}
		// Each pass uses the same total budget, including earlier dependencies.
		// Only the existing broker opens files, after real file_read policy.
		var err error
		content, err = filebroker.CaptureReviewContent(ctx, workspace, cwd, paths, request.MaxBytes, authorize)
		if err != nil {
			return nil, guardian.ContentCoverage{}, err
		}
		for _, path := range paths {
			bodies[path] = content.Bytes(path)
		}
	}
}

func (r *ReviewExecution) Snapshot() guardian.ExecutionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snapshot
	s.Content = slices.Clone(s.Content)
	s.WritePaths = slices.Clone(s.WritePaths)
	s.Missing = slices.Clone(s.Missing)
	return s
}

func (r *ReviewExecution) Content(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.content.Bytes(path)
}

// Candidate binds Runtime's frozen user sources and model/configuration route
// to the exact prepared command. Callers cannot substitute execution metadata.
func (r *ReviewExecution) Candidate(ctx context.Context, reviewID string, authorization guardian.AuthorizationSnapshot, versions guardian.ReviewVersions) (guardian.ReviewCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.used {
		return guardian.ReviewCandidate{}, errors.New("Guardian snapshot is no longer reviewable")
	}
	if err := r.validateContent(ctx); err != nil {
		return guardian.ReviewCandidate{}, err
	}
	prepared, err := r.invocation.SecurityInvocation()
	if err != nil {
		return guardian.ReviewCandidate{}, err
	}
	workspaceID := r.guard.GuardianWorkspaceID()
	facets, effect := r.invocation.Assessment.Facets(), r.invocation.Assessment.Effect()
	versions.PolicyRevision, versions.Permission = r.revision, string(r.permission)
	snapshot := r.snapshot
	snapshot.Content, snapshot.Missing = slices.Clone(snapshot.Content), slices.Clone(snapshot.Missing)
	snapshot.WritePaths = slices.Clone(snapshot.WritePaths)
	authorization.Sources = slices.Clone(authorization.Sources)
	candidate := guardian.ReviewCandidate{
		Identity:  guardian.InvocationIdentity{ReviewID: reviewID, WorkspaceID: workspaceID, WorkspaceGeneration: r.guard.workspaceGeneration, SessionID: r.identity.SessionID, ThreadID: r.identity.ThreadID, TurnID: r.identity.TurnID, CallID: r.invocation.CallID, AttemptID: r.attemptID},
		Binding:   guardian.BindingIdentity{Tool: r.invocation.Tool, CatalogID: r.invocation.Ref.CatalogID, CatalogGeneration: r.invocation.Ref.Generation, Revision: r.invocation.Ref.Revision, Authority: r.invocation.Ref.Authority, ArgumentsDigest: reviewDigest(r.invocation.Arguments), Subject: prepared.Subject},
		Facts:     guardian.CandidateFacts{BuiltinBinding: true, RegisteredForReview: r.invocation.Binding.GuardianReview, Capability: "process", Stage: "call", SandboxRequired: facets.StrongSandbox, HostExecution: facets.HostExecution, NetworkAccess: facets.Network || facets.LoopbackReach || facets.HostLocalTarget, EffectRule: r.invocation.Assessment.Rule(), EffectKind: string(effect.Kind), EffectRisk: string(effect.Risk), EffectReversibility: string(effect.Reversibility), EvidenceComplete: snapshot.Validate() == nil},
		Execution: snapshot, Authorization: authorization, Versions: versions,
	}
	return candidate, candidate.Validate()
}

// GuardianWorkspaceID is the immutable execution authority scope shared by
// source capture and Candidate. Memory and editor URI identities may use other
// namespaces and must not be substituted for this security identity.
func (g *Guard) GuardianWorkspaceID() string {
	if g == nil {
		return ""
	}
	if g.workspaceID != "" {
		return g.workspaceID
	}
	sum := sha256.Sum256([]byte(g.workspace))
	return hex.EncodeToString(sum[:])
}

func (r *ReviewExecution) ValidateInvocation(ctx context.Context, invocation tool.PreparedInvocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.used {
		return errors.New("Guardian execution snapshot is closed or consumed")
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = invocation.CallID
	if identity != r.identity || invocation.Ref.Authority != r.invocation.Ref.Authority || reviewInvocationDigest(invocation) != reviewInvocationDigest(r.invocation) {
		return errors.New("Guardian invocation changed after review")
	}
	if _, err := r.guard.registry.ResolveTrustedBinding(invocation.Ref); err != nil {
		return err
	}
	if r.guard.registry.Generation() != invocation.Ref.Generation {
		return errors.New("Guardian catalog generation changed after review")
	}
	runtime, err := r.guard.samplePolicy()
	if err != nil {
		return err
	}
	if runtime.Revision != r.revision || runtime.Permission != r.permission {
		return errors.New("Guardian policy changed after review")
	}
	plan, err := r.provider.GuardianReviewPlan(invocation)
	if err != nil {
		return err
	}
	if !sameReviewPlan(plan, r.plan) {
		return errors.New("Guardian execution environment changed after review")
	}
	if err := r.snapshot.Validate(); err != nil {
		return err
	}
	return r.validateContent(ctx)
}

func (r *ReviewExecution) Begin(ctx context.Context, id string, scopes []string) (tool.IsolatedWorkspace, error) {
	return r.take(ctx, id, scopes, "apply")
}
func (r *ReviewExecution) BeginShadow(ctx context.Context, id string, scopes []string) (tool.IsolatedWorkspace, error) {
	return r.take(ctx, id, scopes, "discard")
}

func (r *ReviewExecution) take(ctx context.Context, id string, scopes []string, settlement string) (tool.IsolatedWorkspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.used {
		return nil, errors.New("Guardian snapshot may be consumed only once")
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = id
	if identity != r.identity {
		return nil, errors.New("Guardian snapshot belongs to another invocation")
	}
	runtime, err := r.guard.samplePolicy()
	if err != nil {
		return nil, err
	}
	if runtime.Revision != r.revision || runtime.Permission != r.permission {
		return nil, errors.New("Guardian policy changed before snapshot handoff")
	}
	plan, err := r.provider.GuardianReviewPlan(r.invocation)
	if err != nil {
		return nil, err
	}
	if !sameReviewPlan(plan, r.plan) {
		return nil, errors.New("Guardian environment changed before snapshot handoff")
	}
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	if id != r.invocation.CallID || settlement != r.plan.Settlement || !slices.Equal(scopes, r.plan.WritePaths) {
		return nil, errors.New("Guardian snapshot execution scopes changed")
	}
	if err := r.snapshot.Validate(); err != nil {
		return nil, err
	}
	if err := r.validateContent(ctx); err != nil {
		return nil, err
	}
	r.used = true
	return reviewedWorkspace{owner: r, context: ctx}, nil
}

func (r *ReviewExecution) validateContent(ctx context.Context) error {
	if err := r.content.Validate(ctx); err != nil {
		return err
	}
	return r.content.ValidateReadOnly(ctx, r.plan.WritePaths)
}

// withLaunch runs after Shell consumed the copy and prepared the actual sandbox
// command. The launch callback must not reenter ReviewExecution.
func (r *ReviewExecution) withLaunch(ctx context.Context, start func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.used {
		return errors.New("Guardian copy is not ready for process start")
	}
	plan, err := r.provider.GuardianReviewPlan(r.invocation)
	if err != nil {
		return err
	}
	if !sameReviewPlan(plan, r.plan) {
		return errors.New("Guardian environment changed before process start")
	}
	if err := r.validateContent(ctx); err != nil {
		return err
	}
	return start()
}

// Close releases only an unconsumed preparation. After Begin transfers the
// copy, the returned workspace belongs to Shell until final poll or OnClose.
// Guard's deferred cleanup must not reclaim a still-running process's cwd.
func (r *ReviewExecution) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.used {
		return nil
	}
	return r.closeLocked()
}

func (r *ReviewExecution) closeLocked() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.session.Close()
}

type reviewedWorkspace struct {
	owner   *ReviewExecution
	context context.Context
}

func (w reviewedWorkspace) Root() string { return w.owner.snapshot.Root }
func (w reviewedWorkspace) Settle(ctx context.Context) ([]tool.WorkspaceChange, error) {
	return w.owner.session.Settle(ctx)
}
func (w reviewedWorkspace) Close() error {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	return w.owner.closeLocked()
}
func (w reviewedWorkspace) PrepareBackend(parent sandbox.Backend) (sandbox.Backend, func() error, error) {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	current, ok := sandbox.BackendPolicy(parent)
	privatePolicy, valid := sandbox.BackendPolicy(w.owner.backend)
	if w.owner.closed || !ok || !valid || current.ID != w.owner.parentPolicyID || privatePolicy.ID != w.owner.snapshot.SandboxPolicyID {
		return nil, nil, errors.New("Guardian sandbox environment changed")
	}
	if err := w.owner.validateContent(w.context); err != nil {
		return nil, nil, err
	}
	return w.owner.backend, func() error { return nil }, nil
}

func reviewInvocationDigest(invocation Invocation) string {
	return reviewDigest(struct {
		CallID, Tool string
		Ref          tool.ToolRef
		Arguments    json.RawMessage
		Assessment   string
		Binding      tool.TrustedBinding
	}{invocation.CallID, invocation.Tool, invocation.Ref, invocation.Arguments, invocation.Assessment.Digest(), invocation.Binding})
}

func sameReviewPlan(a, b tool.ReviewPlan) bool {
	a.Backend, b.Backend = nil, nil
	return reflect.DeepEqual(a, b)
}

func reviewDigest(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
