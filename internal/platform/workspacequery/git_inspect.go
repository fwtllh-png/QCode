package workspacequery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
)

// GitInspectMaxBytes reuses the workspace resource byte ceiling for each Git
// response. Overflow is an error, never a partially parsed status or patch.
const GitInspectMaxBytes = maxResourceBytes

type GitChangeStat struct {
	Added   int  `json:"added"`
	Removed int  `json:"removed"`
	Binary  bool `json:"binary,omitempty"`
}

type GitChange struct {
	Path      string         `json:"path"`
	Index     string         `json:"index"`
	Worktree  string         `json:"worktree"`
	Untracked bool           `json:"untracked,omitempty"`
	Conflict  bool           `json:"conflict,omitempty"`
	Staged    *GitChangeStat `json:"staged,omitempty"`
	Unstaged  *GitChangeStat `json:"unstaged,omitempty"`
}

type GitOverview struct {
	GitState
	Revision string      `json:"revision"`
	Head     string      `json:"head"`
	Root     bool        `json:"root"`
	Files    []GitChange `json:"files"`
	Remotes  []string    `json:"remotes"`
}

type GitPatch struct {
	Path   string `json:"path"`
	Staged bool   `json:"staged"`
	Diff   string `json:"diff"`
}

// GitOverview reads Git metadata and bounded untracked content on demand.
func (s *Service) GitOverview(ctx context.Context) (GitOverview, error) {
	state, err := s.GitState(ctx)
	result := GitOverview{GitState: state, Files: []GitChange{}, Remotes: []string{}}
	if err != nil || !state.Repository {
		return result, err
	}
	prefix, err := s.gitInspect(ctx, "rev-parse", "--show-prefix")
	if err != nil {
		return GitOverview{}, err
	}
	result.Root = strings.TrimSuffix(prefix, "\n") == ""
	result.Revision, result.Head, err = s.GitRevision(ctx)
	if err != nil {
		return GitOverview{}, err
	}
	status, err := s.gitInspect(ctx, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all", "--", ".")
	if err != nil {
		return GitOverview{}, err
	}
	files, err := parseGitChanges(status, strings.TrimSuffix(prefix, "\n"))
	if err != nil {
		return GitOverview{}, err
	}
	// Match the resource browser's ignore boundary, including force-added files.
	ignored, err := s.gitInspect(ctx, "ls-files", "-z", "--cached", "--ignored", "--exclude-standard")
	if err != nil {
		return GitOverview{}, err
	}
	excluded := make(map[string]bool)
	for _, name := range strings.Split(ignored, "\x00") {
		excluded[name] = true
	}
	staged, err := s.gitNumstat(ctx, true)
	if err != nil {
		return GitOverview{}, err
	}
	unstaged, err := s.gitNumstat(ctx, false)
	if err != nil {
		return GitOverview{}, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return GitOverview{}, err
		}
		if excluded[file.Path] || repowalk.Skippable(file.Path) {
			continue
		}
		if stat, ok := staged[file.Path]; ok {
			file.Staged = &stat
		}
		if stat, ok := unstaged[file.Path]; ok {
			file.Unstaged = &stat
		}
		if file.Untracked {
			file.Unstaged = s.gitUntrackedStat(file.Path)
		}
		result.Files = append(result.Files, file)
	}
	remotes, err := s.gitInspect(ctx, "remote")
	if err != nil {
		return GitOverview{}, err
	}
	result.Remotes = nonemptyLines(remotes)
	if result.Remotes == nil {
		result.Remotes = []string{}
	}
	return result, nil
}

func (s *Service) gitUntrackedStat(name string) *GitChangeStat {
	// Git status already enumerated nonignored literal paths. Read them through
	// the resource boundary without enumerating the whole repository per file.
	content, skipped, err := s.walker.Read(repowalk.Entry{Path: name}, GitInspectMaxBytes)
	if err != nil {
		return nil
	}
	switch skipped {
	case repowalk.SkipNone:
		added := bytes.Count(content.Data, []byte{'\n'})
		if len(content.Data) > 0 && content.Data[len(content.Data)-1] != '\n' {
			added++
		}
		return &GitChangeStat{Added: added}
	case repowalk.SkipBinary:
		return &GitChangeStat{Binary: true}
	default:
		// Keep the file visible, with absent stats meaning unavailable rather
		// than zero. The UI reports these exclusions from the line totals.
		return nil
	}
}

// GitRevision binds explicit actions to HEAD, the entire index and local Git
// configuration. It deliberately does not read working-tree file contents.
func (s *Service) GitRevision(ctx context.Context) (string, string, error) {
	var values []string
	for _, args := range [][]string{
		{"rev-parse", "--revs-only", "--end-of-options", "HEAD"},
		{"branch", "--show-current"},
		{"diff", "--cached", "--raw", "--no-abbrev", "--no-renames", "-z"},
		{"config", "--local", "--null", "--list"},
	} {
		value, err := s.gitInspect(ctx, args...)
		if err != nil {
			return "", "", err
		}
		values = append(values, value)
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), strings.TrimSpace(values[0]), nil
}

func (s *Service) GitConfigRevision(ctx context.Context) (string, error) {
	config, err := s.gitInspect(ctx, "config", "--local", "--null", "--list")
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(config))
	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) GitCurrentBranch(ctx context.Context) (string, error) {
	branch, err := s.gitInspect(ctx, "branch", "--show-current")
	return strings.TrimSpace(branch), err
}

func (s *Service) GitStagedPaths(ctx context.Context) ([]string, error) {
	value, err := s.gitInspect(ctx, "diff", "--cached", "--name-only", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	return gitRecords(value)
}

func (s *Service) GitPatch(ctx context.Context, name string, staged bool) (GitPatch, error) {
	// NUL-delimited Git paths are literal, including tabs, newlines and backslashes.
	// Do not normalize them into another file or accept Git pathspec expressions.
	overview, err := s.GitOverview(ctx)
	if err != nil {
		return GitPatch{}, err
	}
	var change *GitChange
	for index := range overview.Files {
		if overview.Files[index].Path == name {
			change = &overview.Files[index]
			break
		}
	}
	if change == nil {
		return GitPatch{}, os.ErrNotExist
	}
	result := GitPatch{Path: name, Staged: staged}
	if change.Untracked {
		if staged {
			return GitPatch{}, errors.New("untracked file has no staged diff")
		}
		// Resource enforces regular-file, ignore, encoding and size boundaries.
		resource, err := s.Resource(ctx, name)
		if err != nil {
			return GitPatch{}, err
		}
		if resource.Path != name {
			return GitPatch{}, errors.New("untracked path is not canonical")
		}
		lines := strings.Split(strings.TrimSuffix(resource.Content, "\n"), "\n")
		if resource.Content == "" {
			lines = nil
		}
		var patch strings.Builder
		fmt.Fprintf(&patch, "--- /dev/null\n+++ %s\n@@ -0,0 +1,%d @@\n", strconv.Quote("b/"+name), len(lines))
		for _, line := range lines {
			fmt.Fprintf(&patch, "+%s\n", line)
		}
		if resource.Content != "" && !strings.HasSuffix(resource.Content, "\n") {
			patch.WriteString("\\ No newline at end of file\n")
		}
		if patch.Len() > GitInspectMaxBytes {
			return GitPatch{}, errors.New("Git patch exceeds workspace resource byte limit")
		}
		result.Diff = patch.String()
		return result, nil
	}
	args := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--relative"}
	if staged {
		args = append(args, "--cached")
	}
	result.Diff, err = s.gitInspect(ctx, append(args, "--", name)...)
	return result, err
}

func (s *Service) gitNumstat(ctx context.Context, staged bool) (map[string]GitChangeStat, error) {
	args := []string{"diff", "--numstat", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", "--relative"}
	if staged {
		args = append(args, "--cached")
	}
	output, err := s.gitInspect(ctx, append(args, "--", ".")...)
	if err != nil {
		return nil, err
	}
	return parseGitNumstat(output)
}

func parseGitChanges(output, prefix string) ([]GitChange, error) {
	records, err := gitRecords(output)
	if err != nil {
		return nil, err
	}
	files := make([]GitChange, 0, len(records))
	for _, record := range records {
		if len(record) < 4 || record[2] != ' ' {
			return nil, errors.New("invalid Git status record")
		}
		name := strings.TrimPrefix(record[3:], prefix)
		if prefix != "" && !strings.HasPrefix(record[3:], prefix) {
			continue
		}
		code := record[:2]
		conflict := code == "DD" || code == "AU" || code == "UD" || code == "UA" || code == "DU" || code == "AA" || code == "UU"
		files = append(files, GitChange{
			Path: name, Index: string(record[0]), Worktree: string(record[1]),
			Untracked: code == "??", Conflict: conflict,
		})
	}
	return files, nil
}

func parseGitNumstat(output string) (map[string]GitChangeStat, error) {
	records, err := gitRecords(output)
	if err != nil {
		return nil, err
	}
	stats := make(map[string]GitChangeStat, len(records))
	for _, record := range records {
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			return nil, errors.New("invalid Git numstat record")
		}
		stat := GitChangeStat{Binary: parts[0] == "-" && parts[1] == "-"}
		if !stat.Binary {
			stat.Added, err = strconv.Atoi(parts[0])
			if err != nil || stat.Added < 0 {
				return nil, errors.New("invalid Git added count")
			}
			stat.Removed, err = strconv.Atoi(parts[1])
			if err != nil || stat.Removed < 0 {
				return nil, errors.New("invalid Git removed count")
			}
		}
		stats[parts[2]] = stat
	}
	return stats, nil
}

func gitRecords(output string) ([]string, error) {
	if output == "" {
		return nil, nil
	}
	if !utf8.ValidString(output) || !strings.HasSuffix(output, "\x00") {
		return nil, errors.New("Git returned invalid or incomplete UTF-8 records")
	}
	return strings.Split(strings.TrimSuffix(output, "\x00"), "\x00"), nil
}

func (s *Service) gitInspect(ctx context.Context, args ...string) (string, error) {
	directory, err := os.Open(s.workspace.Root())
	if err != nil {
		return "", err
	}
	defer directory.Close()
	result, err := process.Run(ctx, process.Options{
		Path: process.GitExecutable(),
		Args: append([]string{"--no-optional-locks"}, process.ManagedGitArguments(args)...),
		Dir:  s.workspace.Root(), DirFile: directory, OutputLimitBytes: GitInspectMaxBytes,
	})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", errors.New(strings.TrimSpace(result.Stderr))
	}
	if result.OutputReceipt.Stdout.Truncated() {
		return "", errors.New("Git result exceeds workspace resource byte limit")
	}
	if !utf8.ValidString(result.Stdout) {
		return "", errors.New("Git result is not UTF-8 text")
	}
	return result.Stdout, nil
}
