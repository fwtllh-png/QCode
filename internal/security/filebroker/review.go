package filebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ReviewContent retains the exact reviewed bytes from an already private
// execution root. It does not grant reads or make a mutable parent executable.
type ReviewContent struct {
	workspace                      *sandbox.Workspace
	entries                        []guardian.ContentEvidence
	bodies                         map[string][]byte
	rootIdentity, cwdIdentity, cwd string
}

// CaptureReviewContent requires Guard to authorize each source path first.
// maxBytes is an explicit total input budget, never a hidden truncation limit.
func CaptureReviewContent(ctx context.Context, workspace *sandbox.Workspace, cwd string, paths []string, maxBytes int64, authorize func(string) error) (*ReviewContent, error) {
	if workspace == nil || maxBytes <= 0 || authorize == nil {
		return nil, errors.New("review content requires a workspace, read authority and positive byte budget")
	}
	rootID, err := reviewDirectoryIdentity(workspace, ".")
	if err != nil {
		return nil, err
	}
	cwdID, err := reviewDirectoryIdentity(workspace, cwd)
	if err != nil {
		return nil, err
	}
	classifier, err := pathpolicy.ControlPlaneWithin(workspace.Root())
	if err != nil {
		return nil, err
	}
	result := &ReviewContent{workspace: workspace, bodies: make(map[string][]byte), rootIdentity: rootID, cwdIdentity: cwdID, cwd: cwd}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	remaining := maxBytes
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsLocal(path) || filepath.Clean(path) != path || path == "." {
			return nil, errors.New("review content path must be workspace-relative")
		}
		if _, exists := result.bodies[path]; exists {
			return nil, errors.New("duplicate review content path")
		}
		if _, protected, err := classifier.Classify(path); err != nil || protected {
			return nil, errors.New("review content touches a protected path")
		}
		if pathpolicy.IsCredentialFileName(filepath.Base(path)) || pathpolicy.InCredentialLocation(path, "") {
			return nil, errors.New("review content touches a credential location")
		}
		if err := authorize(path); err != nil {
			return nil, err
		}
		entry, body, err := readReviewFile(ctx, workspace, path, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(body))
		result.entries = append(result.entries, entry)
		result.bodies[path] = body
	}
	return result, result.Validate(ctx)
}

func (r *ReviewContent) Entries() []guardian.ContentEvidence {
	return append([]guardian.ContentEvidence(nil), r.entries...)
}
func (r *ReviewContent) Bytes(path string) []byte   { return append([]byte(nil), r.bodies[path]...) }
func (r *ReviewContent) RootIdentity() string       { return r.rootIdentity }
func (r *ReviewContent) WorkingDirIdentity() string { return r.cwdIdentity }

// ValidateReadOnly compares descriptor identities rather than path spelling.
// Case aliases on case-insensitive filesystems must not hide a writable script.
func (r *ReviewContent) ValidateReadOnly(ctx context.Context, scopes []string) error {
	writable := make(map[string]bool)
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := r.workspace.OpenDirectory(scope)
		if err != nil {
			file, err = r.workspace.OpenFile(scope)
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("review write scope identity is unavailable: %s", scope)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(statErr, closeErr); err != nil {
			return err
		}
		identity, err := reviewFileIdentity(info)
		if err != nil {
			return err
		}
		writable[identity] = true
	}
	for _, entry := range r.entries {
		if writable[entry.Identity] {
			return fmt.Errorf("execution content is writable: %s", entry.Path)
		}
		for parent := filepath.Dir(entry.Path); ; parent = filepath.Dir(parent) {
			if err := ctx.Err(); err != nil {
				return err
			}
			identity, err := reviewDirectoryIdentity(r.workspace, parent)
			if err != nil {
				return err
			}
			if writable[identity] {
				return fmt.Errorf("execution content is inside a writable tree: %s", entry.Path)
			}
			if parent == "." {
				break
			}
		}
	}
	return ctx.Err()
}

func (r *ReviewContent) Validate(ctx context.Context) error {
	if r == nil {
		return errors.New("review content is unavailable")
	}
	rootID, err := reviewDirectoryIdentity(r.workspace, ".")
	if err != nil || rootID != r.rootIdentity {
		return errors.New("review execution root changed")
	}
	cwdID, err := reviewDirectoryIdentity(r.workspace, r.cwd)
	if err != nil || cwdID != r.cwdIdentity {
		return errors.New("review execution cwd changed")
	}
	for _, expected := range r.entries {
		actual, _, err := readReviewFile(ctx, r.workspace, expected.Path, expected.Size)
		if err != nil || actual != expected {
			return fmt.Errorf("review content changed: %s", expected.Path)
		}
	}
	return ctx.Err()
}

func readReviewFile(ctx context.Context, w *sandbox.Workspace, path string, limit int64) (guardian.ContentEvidence, []byte, error) {
	file, err := w.OpenFile(path)
	if err != nil {
		return guardian.ContentEvidence{}, nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return guardian.ContentEvidence{}, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return guardian.ContentEvidence{}, nil, errors.New("review content exceeds the supplied byte budget or is not regular")
	}
	reader := &io.LimitedReader{R: reviewReader{ctx, file}, N: limit}
	body, err := io.ReadAll(reader)
	if err != nil {
		return guardian.ContentEvidence{}, nil, err
	}
	var extra [1]byte
	n, tailErr := file.Read(extra[:])
	if n != 0 || tailErr != io.EOF {
		return guardian.ContentEvidence{}, nil, errors.New("review content grew beyond its byte budget")
	}
	after, err := file.Stat()
	if err != nil {
		return guardian.ContentEvidence{}, nil, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		return guardian.ContentEvidence{}, nil, errors.New("review content changed while reading")
	}
	id, err := reviewFileIdentity(after)
	if err != nil {
		return guardian.ContentEvidence{}, nil, err
	}
	sum := sha256.Sum256(body)
	return guardian.ContentEvidence{Path: path, Identity: id, Digest: hex.EncodeToString(sum[:]), Mode: uint32(after.Mode().Perm()), Size: int64(len(body))}, body, nil
}

type reviewReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r reviewReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func reviewDirectoryIdentity(w *sandbox.Workspace, path string) (string, error) {
	file, err := w.OpenDirectory(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	return reviewFileIdentity(info)
}

func reviewFileIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("review file identity is unavailable")
	}
	return strings.Join([]string{fmt.Sprint(stat.Dev), fmt.Sprint(stat.Ino)}, ":"), nil
}
