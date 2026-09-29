// Package vcsbroker owns allowlisted Git metadata mutations.
package vcsbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/processbroker"
)

type MutationKind string

const (
	WorktreeAdd    MutationKind = "worktree_add"
	WorktreeRemove MutationKind = "worktree_remove"
	WorktreePrune  MutationKind = "worktree_prune"
	IndexAdd       MutationKind = "index_add"
	Commit         MutationKind = "commit"
	ApplyPatch     MutationKind = "apply_patch"
	SwitchBranch   MutationKind = "switch_branch"
	CreateBranch   MutationKind = "create_branch"
	Fetch          MutationKind = "fetch"
	Pull           MutationKind = "pull"
	Push           MutationKind = "push"
	Merge          MutationKind = "merge"
	Rebase         MutationKind = "rebase"
	CherryPick     MutationKind = "cherry_pick"
	Restore        MutationKind = "restore"
	StashPush      MutationKind = "stash_push"
	StashPop       MutationKind = "stash_pop"
	Tag            MutationKind = "tag"
	Conflict       MutationKind = "conflict"
)

type RepositoryState struct {
	CommonDirIdentity string `json:"common_dir_identity"`
	HEAD              string `json:"head"`
	Ref               string `json:"ref"`
	IndexDigest       string `json:"index_digest"`
	ConfigDigest      string `json:"config_digest"`
	WorktreesDigest   string `json:"worktrees_digest"`
}

type Mutation struct {
	Kind MutationKind
	Dir  string
	Args []string
}

type Broker struct {
	repository       string
	workspaceID      string
	commonDir        string
	authority        *authority.LeaseAuthority
	processes        *processbroker.Broker
	sequence         atomic.Uint64
	leaseTTL         time.Duration
	mu               sync.Mutex
	beforeRevalidate func() error
}

func New(
	repository string,
	manager *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*Broker, error) {
	if manager == nil {
		return nil, errors.New("VCS Broker requires a Lease Authority")
	}
	if leaseTTL <= 0 {
		return nil, errors.New("VCS Broker requires an explicit Lease TTL")
	}
	canonical, err := filepath.Abs(repository)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return nil, errors.New("VCS Broker repository must be a directory")
	}
	processes, err := processbroker.New(manager)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(canonical)))
	return &Broker{
		repository: canonical, workspaceID: hex.EncodeToString(sum[:]),
		authority: manager, processes: processes, leaseTTL: leaseTTL,
	}, nil
}

func (b *Broker) Read(
	ctx context.Context,
	dir string,
	arguments ...string,
) (string, error) {
	if b == nil {
		return "", errors.New("VCS Broker is required")
	}
	return runGit(trustedReadContext{Context: ctx}, dir, arguments...)
}

func (b *Broker) SwitchBranch(
	ctx context.Context,
	dir string,
	branch string,
) error {
	_, err := b.Mutate(ctx, Mutation{
		Kind: SwitchBranch,
		Dir:  dir,
		Args: []string{"switch", "--no-guess", "--", branch},
	})
	return err
}

func (b *Broker) Mutate(
	ctx context.Context,
	mutation Mutation,
) (processbroker.Result, error) {
	if b == nil || b.authority == nil || b.processes == nil {
		return processbroker.Result{}, errors.New("VCS Broker is required")
	}
	if err := validateMutation(b.repository, mutation); err != nil {
		return processbroker.Result{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	readCtx := trustedReadContext{Context: ctx}
	if remote, push := mutationRemote(mutation); remote != "" {
		arguments := []string{"remote", "get-url"}
		if push {
			arguments = append(arguments, "--push")
		}
		arguments = append(arguments, remote)
		if _, err := runGit(readCtx, mutation.Dir, arguments...); err != nil {
			return processbroker.Result{}, fmt.Errorf(
				"resolve configured Git remote %q: %w", remote, err,
			)
		}
	}
	before, err := b.snapshot(readCtx, mutation.Dir)
	if err != nil {
		return processbroker.Result{}, err
	}
	sequence := b.sequence.Add(1)
	subject, err := authority.NewManagedProcessSubject(
		authority.SubjectHost,
		"vcs-broker",
		authority.TrustHost,
		sequence,
		struct {
			Mutation Mutation        `json:"mutation"`
			Before   RepositoryState `json:"before"`
		}{Mutation: mutation, Before: before},
	)
	if err != nil {
		return processbroker.Result{}, err
	}
	executable := process.GitExecutable()
	arguments := process.ManagedGitArguments(mutation.Args)
	effectKind := securitymodel.ProcessMutating
	reversibility := securitymodel.Bounded
	risk := securitymodel.RiskHigh
	switch mutation.Kind {
	case Fetch:
		effectKind = securitymodel.NetworkRead
		reversibility = securitymodel.Reversible
		risk = securitymodel.RiskMedium
	case Pull:
		effectKind = securitymodel.NetworkMutating
	case Push:
		effectKind = securitymodel.ExternalMutation
		reversibility = securitymodel.Irreversible
	}
	operation, err := authority.BuildManagedProcessOperation(
		authority.ManagedProcessInput{
			ID: fmt.Sprintf("vcs-%d", sequence), Tool: "vcs_broker",
			WorkspaceID: b.workspaceID, WorkspaceGeneration: 1,
			Subject: subject, Executable: executable,
			Args: arguments, WorkingDirectory: mutation.Dir,
			Effect: authority.EffectContract{
				Kind:                 effectKind,
				Reversibility:        reversibility,
				Risk:                 risk,
				WorkspaceTransaction: authority.WorkspaceTransactionNone,
			},
			Required: authority.RequiredControls{},
		},
	)
	if err != nil {
		return processbroker.Result{}, err
	}
	profile, err := authority.BuildManagedProcessProfile(
		authority.ManagedProfileInput{
			Operation: operation, Revision: sequence,
			WorkspaceRoot: b.repository, WorkspaceBaseWrite: true,
			AllowNetwork: true,
			Enforcement:  "none", Backend: "none",
		},
	)
	if err != nil {
		return processbroker.Result{}, err
	}
	expiresAt := time.Now().Add(b.leaseTTL)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	lease, err := b.authority.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 1, Attempt: sequence, ExpiresAt: expiresAt,
	})
	if err != nil {
		return processbroker.Result{}, err
	}
	terminal := false
	defer func() {
		if terminal {
			_ = b.authority.Release(lease)
			return
		}
		if snapshot, snapshotErr := b.authority.Snapshot(lease); snapshotErr == nil &&
			snapshot.State == authority.LeaseIssued {
			_ = b.authority.Revoke(lease)
			_ = b.authority.Release(lease)
		}
	}()
	if b.beforeRevalidate != nil {
		if err := b.beforeRevalidate(); err != nil {
			return processbroker.Result{}, err
		}
	}
	current, err := b.snapshot(readCtx, mutation.Dir)
	if err != nil {
		return processbroker.Result{}, err
	}
	if current != before {
		return processbroker.Result{}, errors.New("repository changed before VCS mutation")
	}
	validation := authority.LeaseValidation{
		Operation: operation, PolicyRevision: 1,
		WorkspaceID: b.workspaceID, WorkspaceGeneration: 1,
		SubjectDigest: subject.Digest, SubjectGeneration: subject.Generation,
		Attempt: sequence,
	}
	result, err := b.processes.RunCommand(ctx, processbroker.CommandRequest{
		Lease: lease, Validation: validation,
		Options: process.Options{
			Path: executable, Args: arguments, Dir: mutation.Dir,
		},
		Identity: processbroker.Identity{
			SessionID: "vcs-broker", ThreadID: operation.ID, TurnID: operation.ID,
		},
	})
	terminal = true
	if err == nil && result.Process.ExitCode != 0 {
		message := strings.TrimSpace(result.Process.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Process.Stdout)
		}
		err = fmt.Errorf(
			"git %s: %s", strings.Join(mutation.Args, " "), message,
		)
	}
	return result, err
}

// trustedReadContext keeps cancellation and deadlines while preventing a
// caller's tool authority from constraining the broker's fixed read-only
// preflight commands. Mutations still execute under a broker-issued lease.
type trustedReadContext struct{ context.Context }

func (trustedReadContext) Value(any) any { return nil }

func (b *Broker) snapshot(
	ctx context.Context,
	dir string,
) (RepositoryState, error) {
	common, err := runGit(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return RepositoryState{}, err
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return RepositoryState{}, err
	}
	info, err := os.Stat(common)
	if err != nil || !info.IsDir() {
		return RepositoryState{}, errors.New("Git common directory is invalid")
	}
	if b.commonDir == "" {
		b.commonDir = common
	} else if b.commonDir != common {
		return RepositoryState{}, errors.New("VCS mutation changed Repository identity")
	}
	head, err := runGit(ctx, dir, "rev-parse", "--revs-only", "--end-of-options", "HEAD")
	if err != nil {
		return RepositoryState{}, err
	}
	ref, _ := runGit(ctx, dir, "symbolic-ref", "-q", "HEAD")
	indexPath, err := runGit(ctx, dir, "rev-parse", "--git-path", "index")
	if err != nil {
		return RepositoryState{}, err
	}
	indexPath = strings.TrimSpace(indexPath)
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(dir, indexPath)
	}
	indexDigest, err := digestFile(indexPath)
	if err != nil {
		return RepositoryState{}, err
	}
	configPath, err := runGit(ctx, dir, "rev-parse", "--git-path", "config")
	if err != nil {
		return RepositoryState{}, err
	}
	configPath = strings.TrimSpace(configPath)
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(dir, configPath)
	}
	configDigest, err := digestFile(configPath)
	if err != nil {
		return RepositoryState{}, err
	}
	worktrees, err := runGit(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return RepositoryState{}, err
	}
	return RepositoryState{
		CommonDirIdentity: fmt.Sprintf(
			"%s:%d:%d", common, info.ModTime().UnixNano(), info.Size(),
		),
		HEAD: strings.TrimSpace(head), Ref: strings.TrimSpace(ref),
		IndexDigest: indexDigest, ConfigDigest: configDigest,
		WorktreesDigest: digestBytes([]byte(worktrees)),
	}, nil
}

func mutationRemote(mutation Mutation) (remote string, push bool) {
	switch mutation.Kind {
	case Fetch:
		return mutation.Args[2], false
	case Pull:
		return mutation.Args[3], false
	case Push:
		return mutation.Args[3], true
	default:
		return "", false
	}
}

func validateMutation(repository string, mutation Mutation) error {
	repository, err := filepath.Abs(repository)
	if err != nil {
		return err
	}
	repository, err = filepath.EvalSymlinks(repository)
	if err != nil {
		return err
	}
	if mutation.Dir == "" {
		mutation.Dir = repository
	}
	directory, err := filepath.Abs(mutation.Dir)
	if err != nil {
		return err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if mutation.Kind == "" || len(mutation.Args) == 0 {
		return errors.New("VCS mutation is incomplete")
	}
	switch mutation.Kind {
	case WorktreeAdd:
		if len(mutation.Args) != 5 ||
			strings.Join(mutation.Args[:3], " ") != "worktree add --detach" {
			return errors.New("invalid worktree add mutation")
		}
	case WorktreeRemove:
		if len(mutation.Args) != 4 ||
			strings.Join(mutation.Args[:3], " ") != "worktree remove --force" {
			return errors.New("invalid worktree remove mutation")
		}
	case WorktreePrune:
		if len(mutation.Args) != 2 ||
			mutation.Args[0] != "worktree" || mutation.Args[1] != "prune" {
			return errors.New("invalid worktree prune mutation")
		}
	case IndexAdd:
		if len(mutation.Args) < 3 ||
			mutation.Args[0] != "add" || mutation.Args[1] != "-A" ||
			mutation.Args[2] != "--" ||
			!validPaths(mutation.Args[3:]) {
			return errors.New("invalid index mutation")
		}
	case Commit:
		if !validCommitArguments(mutation.Args) {
			return errors.New("invalid commit mutation")
		}
	case ApplyPatch:
		if len(mutation.Args) != 3 ||
			mutation.Args[0] != "apply" ||
			mutation.Args[1] != "--whitespace=nowarn" {
			return errors.New("invalid patch mutation")
		}
	case SwitchBranch:
		if len(mutation.Args) != 4 ||
			mutation.Args[0] != "switch" ||
			mutation.Args[1] != "--no-guess" ||
			mutation.Args[2] != "--" ||
			!validBranch(mutation.Args[3]) ||
			directory != repository {
			return errors.New("invalid branch switch mutation")
		}
	case CreateBranch:
		if len(mutation.Args) != 3 ||
			mutation.Args[0] != "switch" ||
			mutation.Args[1] != "-c" ||
			!validBranch(mutation.Args[2]) ||
			directory != repository {
			return errors.New("invalid branch creation mutation")
		}
	case Fetch:
		if len(mutation.Args) != 3 ||
			mutation.Args[0] != "fetch" ||
			mutation.Args[1] != "--prune" ||
			!validRemote(mutation.Args[2]) ||
			directory != repository {
			return errors.New("invalid fetch mutation")
		}
	case Pull:
		if len(mutation.Args) != 5 ||
			mutation.Args[0] != "pull" ||
			mutation.Args[1] != "--ff-only" ||
			mutation.Args[2] != "--" ||
			!validRemote(mutation.Args[3]) ||
			!validBranch(mutation.Args[4]) ||
			directory != repository {
			return errors.New("invalid pull mutation")
		}
	case Push:
		if len(mutation.Args) != 5 ||
			mutation.Args[0] != "push" ||
			mutation.Args[1] != "--porcelain" ||
			mutation.Args[2] != "--" ||
			!validRemote(mutation.Args[3]) ||
			!validPushRefspec(mutation.Args[4]) ||
			directory != repository {
			return errors.New("invalid push mutation")
		}
	case Merge:
		if len(mutation.Args) != 4 ||
			mutation.Args[0] != "merge" ||
			mutation.Args[1] != "--no-edit" ||
			mutation.Args[2] != "--" ||
			!validRevision(mutation.Args[3]) ||
			directory != repository {
			return errors.New("invalid merge mutation")
		}
	case Rebase:
		if len(mutation.Args) != 3 ||
			mutation.Args[0] != "rebase" ||
			mutation.Args[1] != "--" ||
			!validRevision(mutation.Args[2]) ||
			directory != repository {
			return errors.New("invalid rebase mutation")
		}
	case CherryPick:
		if len(mutation.Args) != 3 ||
			mutation.Args[0] != "cherry-pick" ||
			mutation.Args[1] != "--" ||
			!validRevision(mutation.Args[2]) ||
			directory != repository {
			return errors.New("invalid cherry-pick mutation")
		}
	case Restore:
		if !validRestoreArguments(mutation.Args) || directory != repository {
			return errors.New("invalid restore mutation")
		}
	case StashPush:
		if !validStashPushArguments(mutation.Args) || directory != repository {
			return errors.New("invalid stash push mutation")
		}
	case StashPop:
		if len(mutation.Args) != 2 ||
			mutation.Args[0] != "stash" ||
			mutation.Args[1] != "pop" ||
			directory != repository {
			return errors.New("invalid stash pop mutation")
		}
	case Tag:
		if !validTagArguments(mutation.Args) || directory != repository {
			return errors.New("invalid tag mutation")
		}
	case Conflict:
		if !validConflictArguments(mutation.Args) || directory != repository {
			return errors.New("invalid conflict mutation")
		}
	default:
		return errors.New("VCS mutation kind is not allowlisted")
	}
	if directory == "" {
		return errors.New("VCS mutation directory is invalid")
	}
	return nil
}

func validBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	return branch != "" &&
		!strings.HasPrefix(branch, "-") &&
		!strings.ContainsAny(branch, "\x00\r\n~^:?*[\\")
}

func validRevision(revision string) bool {
	revision = strings.TrimSpace(revision)
	return revision != "" &&
		!strings.HasPrefix(revision, "-") &&
		!strings.ContainsAny(revision, "\x00\r\n")
}

func validRestoreArguments(arguments []string) bool {
	offset := 1
	if len(arguments) > 1 && arguments[1] == "--staged" {
		offset++
	}
	return len(arguments) > offset+1 &&
		arguments[0] == "restore" &&
		arguments[offset] == "--" &&
		validPaths(arguments[offset+1:])
}

func validStashPushArguments(arguments []string) bool {
	if len(arguments) == 2 {
		return arguments[0] == "stash" && arguments[1] == "push"
	}
	return len(arguments) == 4 &&
		arguments[0] == "stash" &&
		arguments[1] == "push" &&
		arguments[2] == "-m" &&
		validCommitMessage(arguments[3])
}

func validTagArguments(arguments []string) bool {
	if len(arguments) == 3 {
		return arguments[0] == "tag" &&
			arguments[1] == "--" &&
			validBranch(arguments[2])
	}
	return len(arguments) == 5 &&
		arguments[0] == "tag" &&
		arguments[1] == "-a" &&
		validBranch(arguments[2]) &&
		arguments[3] == "-m" &&
		validCommitMessage(arguments[4])
}

func validConflictArguments(arguments []string) bool {
	joined := strings.Join(arguments, "\x00")
	for _, allowed := range [][]string{
		{"merge", "--abort"},
		{"rebase", "--abort"},
		{"-c", "core.editor=true", "rebase", "--continue"},
		{"cherry-pick", "--abort"},
		{"-c", "core.editor=true", "cherry-pick", "--continue"},
	} {
		if joined == strings.Join(allowed, "\x00") {
			return true
		}
	}
	return false
}

func validRemote(remote string) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" || !asciiAlphaNumeric(remote[0]) {
		return false
	}
	for index := 1; index < len(remote); index++ {
		character := remote[index]
		if !asciiAlphaNumeric(character) &&
			character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func validPushRefspec(refspec string) bool {
	const prefix = "HEAD:refs/heads/"
	return strings.HasPrefix(refspec, prefix) &&
		validBranch(strings.TrimPrefix(refspec, prefix))
}

func validPaths(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if path == "" || filepath.IsAbs(path) ||
			strings.ContainsAny(path, "\x00\r\n") {
			return false
		}
		clean := filepath.Clean(path)
		if clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func validCommitArguments(arguments []string) bool {
	baseline := []string{
		"-c", "user.name=QCode",
		"-c", "user.email=qcode@localhost",
		"commit", "--allow-empty", "--no-gpg-sign", "-m",
		"qcode chat baseline",
	}
	if strings.Join(arguments, "\x00") == strings.Join(baseline, "\x00") {
		return true
	}
	return len(arguments) == 4 &&
		arguments[0] == "commit" &&
		arguments[1] == "--no-gpg-sign" &&
		arguments[2] == "-m" &&
		validCommitMessage(arguments[3]) ||
		len(arguments) == 5 &&
			arguments[0] == "commit" &&
			arguments[1] == "--amend" &&
			arguments[2] == "--no-gpg-sign" &&
			arguments[3] == "-m" &&
			validCommitMessage(arguments[4])
}

func validCommitMessage(message string) bool {
	message = strings.TrimSpace(message)
	return message != "" && len(message) <= 4096 &&
		!strings.ContainsAny(message, "\x00\r")
}

func runGit(ctx context.Context, dir string, arguments ...string) (string, error) {
	result, err := process.Run(ctx, process.Options{
		Path: process.GitExecutable(),
		Args: process.ManagedGitArguments(arguments),
		Dir:  dir,
	})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(arguments, " "), message)
	}
	return result.Stdout, nil
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
