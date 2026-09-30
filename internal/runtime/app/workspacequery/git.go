package workspacequery

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fwtllh-png/QCode/internal/platform/process"
)

type GitState struct {
	Repository bool     `json:"repository"`
	Branch     string   `json:"branch,omitempty"`
	Branches   []string `json:"branches,omitempty"`
	Detached   bool     `json:"detached,omitempty"`
	Dirty      bool     `json:"dirty,omitempty"`
}

func (s *Service) GitState(ctx context.Context) (GitState, error) {
	inside, err := s.git(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return GitState{}, nil
		}
		return GitState{}, err
	}
	if strings.TrimSpace(inside) != "true" {
		return GitState{}, nil
	}
	branches, err := s.git(
		ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads",
	)
	if err != nil {
		return GitState{}, err
	}
	current, currentErr := s.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	state := GitState{
		Repository: true,
		Branches:   nonemptyLines(branches),
		Branch:     strings.TrimSpace(current),
	}
	if currentErr != nil {
		state.Detached = true
		state.Branch, err = s.git(ctx, "rev-parse", "--short", "HEAD")
		if err != nil {
			return GitState{}, err
		}
		state.Branch = strings.TrimSpace(state.Branch)
	} else {
		found := false
		for _, branch := range state.Branches {
			found = found || branch == state.Branch
		}
		if !found && state.Branch != "" {
			state.Branches = append(state.Branches, state.Branch)
		}
	}
	status, err := s.git(ctx, "status", "--porcelain=v1")
	if err != nil {
		return GitState{}, err
	}
	state.Dirty = strings.TrimSpace(status) != ""
	return state, nil
}

func (s *Service) git(
	ctx context.Context,
	arguments ...string,
) (string, error) {
	root := s.workspace.Root()
	directory, err := os.Open(root)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	args := process.ManagedGitArguments(arguments)
	options := process.Options{
		Path: process.GitExecutable(), Args: args,
		Dir: root, DirFile: directory,
	}
	// These fixed metadata queries cannot invoke hooks, filesystem monitors,
	// maintenance, or network operations. Avoid a full workspace link audit
	// before every Git metadata command.
	options.Args = append([]string{"--no-optional-locks"}, args...)
	result, err := process.Run(ctx, options)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return "", fmt.Errorf("git %s: %s", arguments[0], message)
	}
	return result.Stdout, nil
}

func nonemptyLines(value string) []string {
	var result []string
	for line := range strings.SplitSeq(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}
