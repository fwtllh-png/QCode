package shell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func (e *protocolExecutor) GuardianReviewPlan(invocation tool.PreparedInvocation) (tool.ReviewPlan, error) {
	if !e.expand || !invocation.Binding.GuardianReview {
		return tool.ReviewPlan{}, errors.New("command binding does not support Guardian review")
	}
	var input execCommandInput
	if err := json.Unmarshal(invocation.Arguments, &input); err != nil {
		return tool.ReviewPlan{}, err
	}
	if input.ExecutionTarget == "host" || input.AllowLoopback || len(input.NetworkTargets) != 0 {
		return tool.ReviewPlan{}, errors.New("Guardian requires a sandbox command without network")
	}
	cwd, err := e.protocol.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return tool.ReviewPlan{}, err
	}
	policy, ok := sandbox.BackendPolicy(e.protocol.backend)
	if !ok {
		return tool.ReviewPlan{}, errors.New("Guardian requires a prepared sandbox environment")
	}
	env, err := environmentEntries(input.Env)
	if err != nil {
		return tool.ReviewPlan{}, err
	}
	encoded, err := json.Marshal(struct {
		Policy, Interpreter string
		Prepared, Declared  []string
	}{policy.ID, process.POSIXShellPath, policy.EnvironmentValues, env})
	if err != nil {
		return tool.ReviewPlan{}, err
	}
	sum := sha256.Sum256(encoded)
	plan := tool.ReviewPlan{Command: input.Command, WorkingDir: cwd, SourceRoot: e.protocol.workspace.Root(), EnvironmentDigest: hex.EncodeToString(sum[:]), Settlement: input.Settle, Backend: e.protocol.backend}
	if plan.Settlement == "" {
		plan.Settlement = "apply"
	}
	for _, path := range input.WritePaths {
		resolved, err := e.protocol.workspace.Resolve(path, sandbox.AllowMissing)
		if err != nil {
			return tool.ReviewPlan{}, err
		}
		relative, err := filepath.Rel(plan.SourceRoot, resolved)
		if err != nil {
			return tool.ReviewPlan{}, err
		}
		plan.WritePaths = append(plan.WritePaths, relative)
	}
	sort.Strings(plan.WritePaths)
	return plan, nil
}
