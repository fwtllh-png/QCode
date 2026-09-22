// Package repowalk enumerates the files of a workspace under one shared set of
// rules.
//
// It exists because every consumer used to walk the tree for itself: the search
// tools asked git whether each path was ignored, one subprocess per directory
// and per file, while the project map applied a different and shorter skip list.
// Enumeration is now a single `git ls-files` call, so every ignore rule git
// knows about — nested .gitignore files, .git/info/exclude, the user's global
// excludes — applies without this package parsing any of them. Workspaces that
// are not repositories, or where git cannot run, fall back to a plain walk that
// only knows the skip list.
package repowalk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// DefaultMaxFileBytes bounds a single file read when the caller sets no limit.
const DefaultMaxFileBytes = 1 << 20

// Sources report how a file set was produced. SourceGit means git enumerated it,
// so its ignore rules were honoured; SourceWalk means only the skip list was.
const (
	SourceGit  = "git"
	SourceWalk = "walk"
)

// skipped directory names are left out under both sources. Keeping them here
// rather than relying on ignore rules preserves the established behaviour that a
// checked-in vendor directory stays out of search results.
var skippedDirectories = map[string]struct{}{
	".git": {}, ".hg": {}, ".svn": {}, "node_modules": {}, "vendor": {}, "bin": {},
	"target": {},
	// The runtime's own state lives here. Reading it back as workspace content
	// would feed a session its own transcript.
	".qcode": {},
}

// Entry is one candidate file. Path is workspace-relative and slash separated,
// so it can be compared and reported without per-platform normalisation.
type Entry struct {
	Path string
	Size int64
	// Modified comes from the same stat as Size. An incremental consumer uses the
	// pair as a cheap hint that a file is unchanged, then confirms with a digest.
	Modified time.Time
}

// SkipReason says why a file was left out of a read.
type SkipReason string

const (
	SkipNone     SkipReason = ""
	SkipLarge    SkipReason = "large"
	SkipBinary   SkipReason = "binary"
	SkipEncoding SkipReason = "encoding"
	SkipMissing  SkipReason = "missing"
	SkipLinked   SkipReason = "linked"
)

// Skips counts the files enumeration and reads left out, by reason.
type Skips struct {
	Ignored   int `json:"ignored,omitempty"`
	Symlink   int `json:"symlink,omitempty"`
	Irregular int `json:"irregular,omitempty"`
	Large     int `json:"large,omitempty"`
	Binary    int `json:"binary,omitempty"`
	Encoding  int `json:"encoding,omitempty"`
	Missing   int `json:"missing,omitempty"`
	Linked    int `json:"linked,omitempty"`
}

// Add records one skipped file.
func (s *Skips) Add(reason SkipReason) {
	switch reason {
	case SkipLarge:
		s.Large++
	case SkipBinary:
		s.Binary++
	case SkipEncoding:
		s.Encoding++
	case SkipMissing:
		s.Missing++
	case SkipLinked:
		s.Linked++
	}
}

// Listing is an enumerated file set, sorted by path.
type Listing struct {
	Files  []Entry `json:"files"`
	Skips  Skips   `json:"skips"`
	Source string  `json:"source"`
}

// Content is a file body that passed the read policy.
type Content struct {
	Path   string
	Data   []byte
	Digest string
}

// Options carries the seams a caller may need to replace.
type Options struct {
	// Run replaces the git subprocess. Tests set it; production leaves it nil.
	Run func(context.Context, process.Options) (process.Result, error)
}

// Walker enumerates and reads one workspace.
type Walker struct {
	root      string
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	run       func(context.Context, process.Options) (process.Result, error)
}

// New returns a walker for root. The backend is used only to run git under the
// same sandbox as any other child process; a nil backend disables git
// enumeration and leaves the plain walk. A backend already bound to this
// workspace is reused as is, so callers that bound one keep their policy.
func New(root string, backend sandbox.Backend, options ...Options) (*Walker, error) {
	if len(options) > 1 {
		return nil, errors.New("at most one repowalk options value is supported")
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		backend, err = sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: workspace.Root()})
		if err != nil {
			return nil, err
		}
	}
	walker := &Walker{root: workspace.Root(), workspace: workspace, backend: backend, run: process.Run}
	if len(options) == 1 && options[0].Run != nil {
		walker.run = options[0].Run
	}
	return walker, nil
}

// Root is the canonical workspace root.
func (w *Walker) Root() string { return w.root }

// List enumerates every regular file the workspace exposes. Symlinks are never
// followed and never listed, so a link out of the workspace cannot widen the
// set.
func (w *Walker) List(ctx context.Context) (Listing, error) {
	paths, source, pruned, err := w.enumerate(ctx)
	if err != nil {
		return Listing{}, err
	}
	listing := Listing{Source: source, Files: make([]Entry, 0, len(paths))}
	listing.Skips.Ignored = pruned
	seen := make(map[string]struct{}, len(paths))
	for _, relative := range paths {
		if err := ctx.Err(); err != nil {
			return Listing{}, err
		}
		if _, duplicate := seen[relative]; duplicate {
			continue
		}
		seen[relative] = struct{}{}
		if withinSkippedDirectory(relative) {
			listing.Skips.Ignored++
			continue
		}
		info, err := os.Lstat(filepath.Join(w.root, filepath.FromSlash(relative)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// git lists index entries whose file is already deleted.
			listing.Skips.Missing++
			continue
		case err != nil:
			return Listing{}, err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			listing.Skips.Symlink++
		case !info.Mode().IsRegular():
			listing.Skips.Irregular++
		default:
			listing.Files = append(listing.Files, Entry{
				Path: relative, Size: info.Size(), Modified: info.ModTime(),
			})
		}
	}
	sort.Slice(listing.Files, func(i, j int) bool {
		return listing.Files[i].Path < listing.Files[j].Path
	})
	return listing, nil
}

// Read returns the body of entry when it passes the read policy: within
// maxBytes, not binary and valid UTF-8. A rejected file yields its reason rather
// than an error, because a workspace holding an image is not a failure.
func (w *Walker) Read(entry Entry, maxBytes int64) (Content, SkipReason, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	if entry.Size > maxBytes {
		return Content{}, SkipLarge, nil
	}
	file, err := w.workspace.OpenFile(filepath.FromSlash(entry.Path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Content{}, SkipMissing, nil
	case errors.Is(err, sandbox.ErrMultiplyLinked):
		return Content{}, SkipLinked, nil
	case err != nil:
		return Content{}, SkipNone, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return Content{}, SkipNone, readErr
	}
	if closeErr != nil {
		return Content{}, SkipNone, closeErr
	}
	switch {
	case int64(len(data)) > maxBytes:
		return Content{}, SkipLarge, nil
	case IsBinary(data):
		return Content{}, SkipBinary, nil
	case !utf8.Valid(data):
		return Content{}, SkipEncoding, nil
	}
	return Content{Path: entry.Path, Data: data, Digest: Digest(data)}, SkipNone, nil
}

// IsBinary reports content no text consumer should treat as lines.
func IsBinary(data []byte) bool { return strings.IndexByte(string(data), 0) >= 0 }

// Digest is the content identity an incremental consumer compares against.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Skippable reports whether a workspace-relative path is left out of every
// listing, so callers that already hold a path can apply the same rule.
func Skippable(relative string) bool { return withinSkippedDirectory(relative) }

func withinSkippedDirectory(relative string) bool {
	for _, segment := range strings.Split(relative, "/") {
		if _, found := skippedDirectories[segment]; found {
			return true
		}
	}
	return false
}

// enumerate returns the candidate paths, the source that produced them and how
// many directories the plain walk pruned. Under SourceGit the pruning shows up
// per file instead, because git lists the files a skipped directory holds.
func (w *Walker) enumerate(ctx context.Context) ([]string, string, int, error) {
	paths, listed, err := w.gitFiles(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	if listed {
		return paths, SourceGit, 0, nil
	}
	paths, pruned, err := w.walkFiles(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	return paths, SourceWalk, pruned, nil
}

// gitFiles asks git for the tracked and untracked-but-not-ignored files. It
// reports listed=false whenever git cannot answer — no repository, no binary, no
// workspace policy — because losing ignore rules is a worse outcome than losing
// the ability to search at all.
func (w *Walker) gitFiles(ctx context.Context) ([]string, bool, error) {
	if w.backend == nil {
		return nil, false, nil
	}
	pinned, err := process.OpenPinnedDirectory(w.backend, w.root)
	if err != nil {
		return nil, false, nil
	}
	defer pinned.Close()
	result, err := w.run(ctx, process.Options{
		Path: "git",
		Args: []string{"ls-files", "--cached", "--others", "--exclude-standard", "-z"},
		Dir:  w.root, DirFile: pinned, Sandbox: w.backend, RequireSandbox: true,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	if err != nil || result.ExitCode != 0 {
		return nil, false, nil
	}
	ignoredResult, err := w.run(ctx, process.Options{
		Path: "git",
		Args: []string{
			"ls-files", "--cached", "--ignored", "--exclude-standard", "-z",
		},
		Dir: w.root, DirFile: pinned, Sandbox: w.backend, RequireSandbox: true,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	if err != nil || ignoredResult.ExitCode != 0 {
		return nil, false, nil
	}
	ignored := make(map[string]struct{})
	for _, field := range strings.Split(ignoredResult.Stdout, "\x00") {
		if field != "" {
			ignored[filepath.ToSlash(field)] = struct{}{}
		}
	}
	fields := strings.Split(result.Stdout, "\x00")
	paths := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}
		relative := filepath.ToSlash(field)
		if _, excluded := ignored[relative]; excluded {
			continue
		}
		paths = append(paths, relative)
	}
	return paths, true, nil
}

func (w *Walker) walkFiles(ctx context.Context) ([]string, int, error) {
	var paths []string
	pruned := 0
	err := filepath.WalkDir(w.root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(w.root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && withinSkippedDirectory(relative) {
				pruned++
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return paths, pruned, nil
}
