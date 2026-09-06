// Package filebroker owns authorized workspace file transactions.
package filebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const PlanVersion = 1

type State struct {
	Exists   bool   `json:"exists"`
	Digest   string `json:"digest,omitempty"`
	Identity string `json:"identity,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
}

type Entry struct {
	Path       string `json:"path"`
	Before     State  `json:"before"`
	BeforeData []byte `json:"before_data,omitempty"`
	After      State  `json:"after"`
	Data       []byte `json:"data,omitempty"`
}

type Plan struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
	Digest  string  `json:"digest"`
}

type Journal interface {
	Before(context.Context, string) error
	After(string) error
}

type Request struct {
	Lease      authority.ExecutionLease
	Validation authority.LeaseValidation
	Plan       Plan
	Journal    Journal
}

type Result struct {
	Before     map[string]State
	After      map[string]State
	Settlement authority.Settlement
}

type Broker struct {
	workspace   *sandbox.Workspace
	authority   *authority.LeaseAuthority
	now         func() time.Time
	beforeApply func(string) error
}

func New(
	workspace *sandbox.Workspace,
	manager *authority.LeaseAuthority,
) (*Broker, error) {
	if workspace == nil || manager == nil {
		return nil, errors.New("file broker requires Workspace and Lease Authority")
	}
	return &Broker{
		workspace: workspace, authority: manager, now: time.Now,
	}, nil
}

func NewPlan(entries []Entry) (Plan, error) {
	plan := Plan{Version: PlanVersion, Entries: cloneEntries(entries)}
	for index := range plan.Entries {
		plan.Entries[index].Path = filepath.ToSlash(
			filepath.Clean(plan.Entries[index].Path),
		)
	}
	sort.Slice(plan.Entries, func(i, j int) bool {
		return plan.Entries[i].Path < plan.Entries[j].Path
	})
	digest, err := planDigest(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Digest = digest
	return plan, plan.Validate()
}

func PlanWrite(
	workspace *sandbox.Workspace,
	path string,
	data []byte,
	mode fs.FileMode,
) (Plan, error) {
	if workspace == nil {
		return Plan{}, errors.New("file transaction Workspace is required")
	}
	before, err := workspace.SnapshotFile(path)
	if err != nil {
		return Plan{}, err
	}
	if before.Exists {
		mode = before.Mode
	}
	sum := sha256.Sum256(data)
	return NewPlan([]Entry{{
		Path: path,
		Before: State{
			Exists: before.Exists, Digest: before.Digest,
			Identity: before.Identity, Mode: uint32(before.Mode.Perm()),
		},
		BeforeData: before.Data,
		After: State{
			Exists: true, Digest: hex.EncodeToString(sum[:]),
			Mode: uint32(mode.Perm()),
		},
		Data: append([]byte(nil), data...),
	}})
}

func (p Plan) Validate() error {
	if p.Version != PlanVersion || !validDigest(p.Digest) {
		return errors.New("file transaction plan is incomplete")
	}
	previous := ""
	for _, entry := range p.Entries {
		if err := validateEntry(entry); err != nil {
			return err
		}
		if previous != "" && entry.Path <= previous {
			return errors.New("file transaction paths are not unique and sorted")
		}
		previous = entry.Path
	}
	expected, err := planDigest(p)
	if err != nil {
		return err
	}
	if expected != p.Digest {
		return errors.New("file transaction plan digest mismatch")
	}
	return nil
}

func (b *Broker) Commit(
	ctx context.Context,
	request Request,
) (Result, error) {
	if b == nil || b.workspace == nil || b.authority == nil {
		return Result{}, errors.New("file broker is required")
	}
	if err := request.Plan.Validate(); err != nil {
		return Result{}, err
	}
	if err := validateOperation(request.Validation.Operation, request.Plan); err != nil {
		return Result{}, err
	}
	var result Result
	settlement, err := b.authority.RunSettled(
		request.Lease,
		request.Validation,
		"file_commit_failed",
		b.now,
		func(settlement *authority.Settlement) error {
			var commitErr error
			result, commitErr = b.commitConsumed(ctx, request, settlement)
			return commitErr
		},
	)
	result.Settlement = settlement
	return result, err
}

func (b *Broker) commitConsumed(
	ctx context.Context,
	request Request,
	settlement *authority.Settlement,
) (Result, error) {
	result := Result{
		Before: make(map[string]State, len(request.Plan.Entries)),
		After:  make(map[string]State, len(request.Plan.Entries)),
	}
	for _, entry := range request.Plan.Entries {
		current, err := b.snapshot(entry.Path)
		if err != nil {
			return result, fmt.Errorf("snapshot %q: %w", entry.Path, err)
		}
		if current != entry.Before {
			return result, fmt.Errorf("file transaction %q is stale", entry.Path)
		}
		result.Before[entry.Path] = current
		if request.Journal != nil {
			if err := request.Journal.Before(
				ctx, filepath.Join(b.workspace.Root(), filepath.FromSlash(entry.Path)),
			); err != nil {
				return result, fmt.Errorf("journal before-image %q: %w", entry.Path, err)
			}
		}
		current, err = b.snapshot(entry.Path)
		if err != nil {
			return result, fmt.Errorf("revalidate %q: %w", entry.Path, err)
		}
		if current != entry.Before {
			return result, fmt.Errorf("file transaction %q changed during preparation", entry.Path)
		}
	}

	var done []Entry
	for _, deletes := range []bool{false, true} {
		for _, entry := range request.Plan.Entries {
			if deletes == entry.After.Exists {
				continue
			}
			if err := ctx.Err(); err != nil {
				settlement.Reason = "context_canceled"
				return result, b.rollback(request, done, err)
			}
			if b.beforeApply != nil {
				if err := b.beforeApply(entry.Path); err != nil {
					return result, b.rollback(request, done, err)
				}
			}
			current, err := b.snapshot(entry.Path)
			if err != nil {
				return result, b.rollback(
					request, done,
					fmt.Errorf("revalidate before apply %q: %w", entry.Path, err),
				)
			}
			if current != entry.Before {
				return result, b.rollback(
					request, done,
					fmt.Errorf("file transaction %q changed before apply", entry.Path),
				)
			}
			if err := b.apply(entry); err != nil {
				return result, b.rollback(
					request, done,
					fmt.Errorf("apply %q: %w", entry.Path, err),
				)
			}
			done = append(done, entry)
		}
	}
	for _, entry := range request.Plan.Entries {
		after, err := b.snapshot(entry.Path)
		if err != nil {
			return result, b.rollback(
				request, done,
				fmt.Errorf("snapshot committed %q: %w", entry.Path, err),
			)
		}
		if !sameContent(after, entry.After) {
			return result, b.rollback(
				request, done,
				fmt.Errorf("file transaction result %q does not match plan", entry.Path),
			)
		}
		result.After[entry.Path] = after
		if request.Journal != nil {
			if err := request.Journal.After(
				filepath.Join(b.workspace.Root(), filepath.FromSlash(entry.Path)),
			); err != nil {
				return result, b.rollback(
					request, done,
					fmt.Errorf("journal settlement %q: %w", entry.Path, err),
				)
			}
		}
	}
	settlement.Status, settlement.Reason = "succeeded", "file_transaction_committed"
	return result, nil
}

func (b *Broker) rollback(
	request Request,
	done []Entry,
	cause error,
) error {
	var fileErrors, journalErrors []error
	for index := len(done) - 1; index >= 0; index-- {
		entry := done[index]
		current, err := b.snapshot(entry.Path)
		if err != nil {
			fileErrors = append(fileErrors, err)
			continue
		}
		if !sameContent(current, entry.After) {
			fileErrors = append(
				fileErrors,
				fmt.Errorf("%s changed after broker write", entry.Path),
			)
			continue
		}
		if entry.Before.Exists {
			err = b.workspace.AtomicWrite(
				entry.Path, entry.BeforeData, fs.FileMode(entry.Before.Mode),
			)
		} else {
			err = b.workspace.Remove(entry.Path)
		}
		if err != nil {
			fileErrors = append(fileErrors, fmt.Errorf("%s: %w", entry.Path, err))
		}
	}
	if request.Journal != nil {
		for _, entry := range request.Plan.Entries {
			if err := request.Journal.After(
				filepath.Join(b.workspace.Root(), filepath.FromSlash(entry.Path)),
			); err != nil {
				journalErrors = append(
					journalErrors,
					fmt.Errorf("journal rollback %s: %w", entry.Path, err),
				)
			}
		}
	}
	if len(fileErrors) != 0 {
		cause = errors.Join(
			cause,
			fmt.Errorf("workspace is partially changed because rollback failed: %w",
				errors.Join(fileErrors...)),
		)
	}
	if len(journalErrors) != 0 {
		cause = errors.Join(
			cause,
			fmt.Errorf("journal rollback settlement failed: %w",
				errors.Join(journalErrors...)),
		)
	}
	return cause
}

func (b *Broker) apply(entry Entry) error {
	if !entry.After.Exists {
		return b.workspace.Remove(entry.Path)
	}
	mode := fs.FileMode(entry.After.Mode)
	if entry.Before.Exists {
		return b.workspace.AtomicWrite(entry.Path, entry.Data, mode)
	}
	return b.workspace.AtomicCreate(entry.Path, entry.Data, mode)
}

func (b *Broker) snapshot(path string) (State, error) {
	value, err := b.workspace.SnapshotFile(path)
	if err != nil {
		return State{}, err
	}
	return State{
		Exists: value.Exists, Digest: value.Digest,
		Identity: value.Identity, Mode: uint32(value.Mode.Perm()),
	}, nil
}

func validateOperation(operation authority.ExecutionOperation, plan Plan) error {
	if err := operation.Validate(); err != nil {
		return err
	}
	if operation.File == nil || operation.File.MutationDigest != plan.Digest {
		return errors.New("file plan does not match the execution operation")
	}
	resources := make(map[string]bool)
	for _, resource := range operation.Resources {
		if resource.Namespace != authority.NamespaceWorkspace ||
			resource.RootID != operation.WorkspaceID ||
			resource.RootGeneration != operation.WorkspaceGeneration ||
			(resource.Access != tool.AccessWrite && resource.Access != tool.AccessTree) {
			continue
		}
		resources[filepath.ToSlash(filepath.Clean(resource.RelativePath))] = true
	}
	for _, entry := range plan.Entries {
		if !resources[entry.Path] {
			return fmt.Errorf("file plan path %q has no exact write resource", entry.Path)
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if entry.Path == "" || entry.Path == "." || filepath.IsAbs(entry.Path) ||
		entry.Path == ".." || strings.HasPrefix(entry.Path, "../") ||
		strings.Contains(entry.Path, `\`) {
		return errors.New("file transaction path is invalid")
	}
	first := strings.SplitN(entry.Path, "/", 2)[0]
	for _, protected := range []string{
		".git", ".qcode", ".qcode-worktree", ".agents", ".codex",
	} {
		if strings.EqualFold(first, protected) {
			return errors.New("file transaction cannot write Workspace control metadata")
		}
	}
	if entry.Before.Exists &&
		(!validDigest(entry.Before.Digest) || entry.Before.Identity == "") {
		return errors.New("file transaction before-state is incomplete")
	}
	if entry.Before.Exists {
		sum := sha256.Sum256(entry.BeforeData)
		if entry.Before.Digest != hex.EncodeToString(sum[:]) {
			return errors.New("file transaction before-content digest mismatch")
		}
	} else if len(entry.BeforeData) != 0 {
		return errors.New("missing file transaction carries before-content")
	}
	if entry.After.Exists {
		sum := sha256.Sum256(entry.Data)
		if !validDigest(entry.After.Digest) ||
			entry.After.Digest != hex.EncodeToString(sum[:]) ||
			entry.After.Mode == 0 {
			return errors.New("file transaction after-state is incomplete")
		}
	} else if len(entry.Data) != 0 {
		return errors.New("deleted file transaction carries content")
	}
	if sameContent(entry.Before, entry.After) {
		return errors.New("file transaction entry has no content change")
	}
	return nil
}

func sameContent(left, right State) bool {
	return left.Exists == right.Exists &&
		(!left.Exists || (left.Digest == right.Digest && left.Mode == right.Mode))
}

func planDigest(plan Plan) (string, error) {
	plan.Digest = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func cloneEntries(entries []Entry) []Entry {
	result := make([]Entry, len(entries))
	for index, entry := range entries {
		result[index] = entry
		result[index].BeforeData = append([]byte(nil), entry.BeforeData...)
		result[index].Data = append([]byte(nil), entry.Data...)
	}
	return result
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
