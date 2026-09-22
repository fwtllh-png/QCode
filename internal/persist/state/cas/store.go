// Package cas provides a durable, disk-backed content-addressed store.
package cas

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	// ErrNotFound indicates that an ID has no committed reference metadata.
	ErrNotFound = errors.New("content not found")
	// ErrClosed indicates that the store has been closed.
	ErrClosed = errors.New("content store is closed")
	// ErrInvalidID indicates that an ID is not a canonical SHA-256 digest.
	ErrInvalidID = errors.New("invalid content ID")
	// ErrDigestMismatch indicates that bytes do not match their content ID.
	ErrDigestMismatch = errors.New("content digest mismatch")
	// ErrCorrupt indicates malformed metadata or tampered on-disk content.
	ErrCorrupt = errors.New("content store is corrupt")
	// ErrReferenceOverflow indicates that a reference count cannot be incremented.
	ErrReferenceOverflow = errors.New("content reference count overflow")
)

const (
	objectsDir      = "objects"
	referencesDir   = "refs"
	lockName        = ".lock"
	metadataVersion = "v1"
	copyBufferSize  = 64 << 10
)

// Store is a durable content-addressed store rooted at a directory.
//
// Store serializes operations within an instance and uses a filesystem lock to
// coordinate with other Store instances opened on the same root.
type Store struct {
	database *sql.DB
	mu       sync.Mutex
	root     *os.Root
	lock     *os.File
	rootPath string
	closed   bool
}

// Open opens or creates a store at root.
func Open(root string) (_ *Store, retErr error) {
	if root == "" {
		return nil, errors.New("CAS root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve CAS root: %w", err)
	}
	absolute = filepath.Clean(absolute)

	if err := createRoot(absolute); err != nil {
		return nil, err
	}
	rootFS, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open CAS root: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = rootFS.Close()
		}
	}()

	lock, err := openLockFile(rootFS)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()

	store := &Store{root: rootFS, lock: lock, rootPath: absolute}
	if err := lockFile(context.Background(), lock, true); err != nil {
		return nil, fmt.Errorf("lock CAS root: %w", err)
	}
	defer func() {
		unlockErr := unlockFile(lock)
		if retErr == nil && unlockErr != nil {
			retErr = fmt.Errorf("unlock CAS root: %w", unlockErr)
		}
	}()
	if err := store.ensureLayout(); err != nil {
		return nil, err
	}
	return store, nil
}

// ID returns the canonical lowercase SHA-256 ID for data.
func ID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Root returns the absolute directory anchoring the store.
func (s *Store) Root() string {
	return s.rootPath
}

// Put stores data and acquires one reference. Repeated puts of the same ID
// deduplicate the content and acquire additional references.
func (s *Store) Put(ctx context.Context, id string, data []byte) error {
	if s.database != nil {
		return s.putSQL(ctx, id, data)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	content := append([]byte(nil), data...)
	digest, err := digestBytes(ctx, content)
	if err != nil {
		return err
	}
	if digest != id {
		return fmt.Errorf("%w: ID %q does not identify supplied bytes", ErrDigestMismatch, id)
	}

	return s.exclusive(ctx, func() error {
		refs, metadataExists, err := s.readReferences(ctx, id)
		if err != nil {
			return err
		}
		stored, objectExists, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		if metadataExists && !objectExists {
			return corruptf("reference metadata exists without object %q", id)
		}
		if objectExists && ID(stored) != id {
			return tamperedf("object %q has a different digest", id)
		}
		if !objectExists {
			if err := s.writeObject(ctx, id, content); err != nil {
				return err
			}
		}
		if !metadataExists {
			refs = 0
		}
		if refs == math.MaxUint64 {
			return ErrReferenceOverflow
		}
		return s.writeReferences(ctx, id, refs+1)
	})
}

// Get returns a copy of the content. It verifies the SHA-256 digest on every
// read and reports corruption rather than returning tampered bytes.
func (s *Store) Get(ctx context.Context, id string) ([]byte, error) {
	if s.database != nil {
		return GetTx(ctx, s.database, id)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := validateID(id); err != nil {
		return nil, err
	}

	var result []byte
	err := s.shared(ctx, func() error {
		if _, exists, err := s.readReferences(ctx, id); err != nil {
			return err
		} else if !exists {
			return ErrNotFound
		}
		content, exists, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return corruptf("reference metadata exists without object %q", id)
		}
		if ID(content) != id {
			return tamperedf("object %q has a different digest", id)
		}
		result = content
		return nil
	})
	return result, err
}

// References reports how many references id currently holds. Callers that own
// the lifetime of what they store need this to know when a Release left content
// unreferenced, since Release itself keeps zero-reference content on disk.
func (s *Store) References(ctx context.Context, id string) (uint64, error) {
	if s.database != nil {
		return s.referencesSQL(ctx, id)
	}
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if err := validateID(id); err != nil {
		return 0, err
	}
	var count uint64
	err := s.shared(ctx, func() error {
		refs, exists, err := s.readReferences(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		count = refs
		return nil
	})
	return count, err
}

// Retain acquires one additional reference to existing content.
func (s *Store) Retain(ctx context.Context, id string) error {
	if s.database != nil {
		return s.adjustSQL(ctx, id, 1)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	return s.exclusive(ctx, func() error {
		refs, exists, err := s.readReferences(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		content, objectExists, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		if !objectExists {
			return corruptf("reference metadata exists without object %q", id)
		}
		if ID(content) != id {
			return tamperedf("object %q has a different digest", id)
		}
		if refs == math.MaxUint64 {
			return ErrReferenceOverflow
		}
		return s.writeReferences(ctx, id, refs+1)
	})
}

// Release relinquishes one reference. Releasing an already-zero count is
// idempotent; zero-reference content remains available until Delete.
func (s *Store) Release(ctx context.Context, id string) error {
	if s.database != nil {
		return s.adjustSQL(ctx, id, -1)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	return s.exclusive(ctx, func() error {
		refs, exists, err := s.readReferences(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		content, objectExists, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		if !objectExists {
			return corruptf("reference metadata exists without object %q", id)
		}
		if ID(content) != id {
			return tamperedf("object %q has a different digest", id)
		}
		if refs > 0 {
			refs--
		}
		return s.writeReferences(ctx, id, refs)
	})
}

// Delete removes content and its reference metadata regardless of the current
// reference count.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.database != nil {
		return s.deleteSQL(ctx, id)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	return s.exclusive(ctx, func() error {
		if _, exists, err := s.readReferences(ctx, id); err != nil {
			return err
		} else if !exists {
			return ErrNotFound
		}
		return s.removeCommitted(ctx, id)
	})
}

// CollectIfUnreferenced deletes id only when its reference count is already
// zero. A live reference is left untouched, including when another caller
// retains the object between a prior Release and this check.
func (s *Store) CollectIfUnreferenced(ctx context.Context, id string) error {
	if s.database != nil {
		_, err := s.collectSQL(ctx, id)
		return err
	}
	_, err := s.collectIfUnreferenced(ctx, id)
	return err
}

// CollectUnreferenced deletes every object whose reference count is zero.
// The only collection condition is the reference count; there is no grace
// period or deletion cap. Each object is digest-verified before removal.
func (s *Store) CollectUnreferenced(ctx context.Context) (int, error) {
	if s.database != nil {
		return s.collectSQL(ctx, "")
	}
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	var ids []string
	err := s.shared(ctx, func() error {
		var listErr error
		ids, listErr = s.listReferenceIDs(ctx)
		return listErr
	})
	if err != nil {
		return 0, err
	}
	collected := 0
	for _, id := range ids {
		deleted, err := s.collectIfUnreferenced(ctx, id)
		if err != nil {
			return collected, err
		}
		if deleted {
			collected++
		}
	}
	return collected, nil
}

// ReleaseUnreferenced releases one reference and deletes the object if that
// leaves it unreferenced. Release itself still keeps zero-reference content
// until this, CollectIfUnreferenced, or CollectUnreferenced runs.
func (s *Store) ReleaseUnreferenced(ctx context.Context, id string) error {
	if err := s.Release(ctx, id); err != nil {
		return err
	}
	return s.CollectIfUnreferenced(ctx, id)
}

func (s *Store) collectIfUnreferenced(ctx context.Context, id string) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if err := validateID(id); err != nil {
		return false, err
	}
	deleted := false
	err := s.exclusive(ctx, func() error {
		refs, exists, err := s.readReferences(ctx, id)
		if err != nil {
			return err
		}
		if !exists || refs != 0 {
			return nil
		}
		if err := s.removeCommitted(ctx, id); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

func (s *Store) removeCommitted(ctx context.Context, id string) error {
	content, exists, err := s.readObject(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return corruptf("reference metadata exists without object %q", id)
	}
	if ID(content) != id {
		return tamperedf("object %q has a different digest", id)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}

	refPath := referencePath(id)
	if err := s.root.Remove(refPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("remove reference metadata: %w", err)
	}
	if err := s.syncDir(referenceParent(id)); err != nil {
		return fmt.Errorf("sync reference directory: %w", err)
	}

	if err := s.root.Remove(objectPath(id)); err != nil {
		return fmt.Errorf("%w: remove object after metadata: %v", ErrCorrupt, err)
	}
	if err := s.syncDir(objectParent(id)); err != nil {
		return fmt.Errorf("sync object directory: %w", err)
	}
	return nil
}

func (s *Store) listReferenceIDs(ctx context.Context) ([]string, error) {
	shards, err := readRootDir(s.root, referencesDir)
	if err != nil {
		return nil, fmt.Errorf("list CAS references: %w", err)
	}
	ids := make([]string, 0)
	for _, shard := range shards {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !shard.IsDir() {
			continue
		}
		name := shard.Name()
		if len(name) != 2 || validateID(name+strings.Repeat("0", 62)) != nil {
			continue
		}
		entries, err := readRootDir(s.root, referencesDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("list CAS reference shard %q: %w", name, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			file := entry.Name()
			if !strings.HasSuffix(file, ".ref") {
				continue
			}
			id := name + strings.TrimSuffix(file, ".ref")
			if err := validateID(id); err != nil {
				return nil, corruptf("reference metadata name %q is not a content ID", file)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func readRootDir(root *os.Root, name string) ([]os.DirEntry, error) {
	directory, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

// Close closes the store. It is safe to call Close repeatedly.
func (s *Store) Close(ctx context.Context) error {
	if s.database != nil {
		return nil // The state store owns the shared database.
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.lock.Close(), s.root.Close())
}

func (s *Store) shared(ctx context.Context, operation func() error) error {
	return s.withLock(ctx, false, operation)
}

func (s *Store) exclusive(ctx context.Context, operation func() error) error {
	return s.withLock(ctx, true, operation)
}

func (s *Store) withLock(ctx context.Context, exclusive bool, operation func() error) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := lockFile(ctx, s.lock, exclusive); err != nil {
		return err
	}
	defer func() {
		_ = unlockFile(s.lock)
	}()
	if err := checkContext(ctx); err != nil {
		return err
	}
	return operation()
}

func createRoot(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("CAS root must not be a symbolic link")
		}
		if !info.IsDir() {
			return errors.New("CAS root is not a directory")
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect CAS root: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create CAS root: %w", err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect created CAS root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("created CAS root is not a secure directory")
	}
	return nil
}

func openLockFile(root *os.Root) (*os.File, error) {
	info, err := root.Lstat(lockName)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("CAS lock is not a regular file")
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("inspect CAS lock: %w", err)
	}
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open CAS lock: %w", err)
	}
	openedInfo, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("inspect opened CAS lock: %w", err)
	}
	pathInfo, err := root.Lstat(lockName)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("reinspect CAS lock: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, openedInfo) {
		_ = lock.Close()
		return nil, errors.New("CAS lock changed while opening")
	}
	return lock, nil
}

func (s *Store) ensureLayout() error {
	for _, name := range []string{objectsDir, referencesDir} {
		created, err := s.ensureDirectory(name)
		if err != nil {
			return err
		}
		if created {
			if err := s.syncDir("."); err != nil {
				return fmt.Errorf("sync CAS root: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) ensureShard(parent, shard string) error {
	name := parent + "/" + shard
	created, err := s.ensureDirectory(name)
	if err != nil {
		return err
	}
	if created {
		if err := s.syncDir(parent); err != nil {
			return fmt.Errorf("sync shard parent: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureDirectory(name string) (bool, error) {
	err := s.root.Mkdir(name, 0o700)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("create CAS directory %q: %w", name, err)
	}
	info, statErr := s.root.Lstat(name)
	if statErr != nil {
		return false, fmt.Errorf("inspect CAS directory %q: %w", name, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("CAS path %q is not a secure directory", name)
	}
	return false, nil
}

func (s *Store) readObject(ctx context.Context, id string) ([]byte, bool, error) {
	content, exists, err := s.readRegularFile(ctx, objectPath(id), -1)
	if err != nil {
		return nil, false, fmt.Errorf("read object %q: %w", id, err)
	}
	return content, exists, nil
}

func (s *Store) writeObject(ctx context.Context, id string, data []byte) error {
	if err := s.ensureShard(objectsDir, id[:2]); err != nil {
		return err
	}
	if err := s.atomicWrite(ctx, objectParent(id), objectPath(id), data); err != nil {
		return fmt.Errorf("write object %q: %w", id, err)
	}
	return nil
}

func (s *Store) readReferences(ctx context.Context, id string) (uint64, bool, error) {
	data, exists, err := s.readRegularFile(ctx, referencePath(id), 64)
	if err != nil {
		return 0, false, fmt.Errorf("read references for %q: %w", id, err)
	}
	if !exists {
		return 0, false, nil
	}
	prefix := metadataVersion + " "
	text := string(data)
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "\n") ||
		strings.Count(text, "\n") != 1 {
		return 0, false, corruptf("invalid reference metadata for %q", id)
	}
	countText := strings.TrimSuffix(strings.TrimPrefix(text, prefix), "\n")
	if countText == "" || (len(countText) > 1 && countText[0] == '0') {
		return 0, false, corruptf("invalid reference count for %q", id)
	}
	refs, err := strconv.ParseUint(countText, 10, 64)
	if err != nil {
		return 0, false, corruptf("invalid reference count for %q", id)
	}
	return refs, true, nil
}

func (s *Store) writeReferences(ctx context.Context, id string, refs uint64) error {
	if err := s.ensureShard(referencesDir, id[:2]); err != nil {
		return err
	}
	data := []byte(metadataVersion + " " + strconv.FormatUint(refs, 10) + "\n")
	if err := s.atomicWrite(ctx, referenceParent(id), referencePath(id), data); err != nil {
		return fmt.Errorf("write references for %q: %w", id, err)
	}
	return nil
}

func (s *Store) readRegularFile(
	ctx context.Context,
	name string,
	maxBytes int64,
) ([]byte, bool, error) {
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, corruptf("%q is not a regular file", name)
	}
	if maxBytes >= 0 && info.Size() > maxBytes {
		return nil, false, corruptf("%q exceeds its size limit", name)
	}

	file, err := s.root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, false, corruptf("%q changed while opening", name)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, false, corruptf("%q changed while opening", name)
	}

	var result bytes.Buffer
	if openedInfo.Size() > 0 && openedInfo.Size() <= int64(maxInt()) {
		result.Grow(int(openedInfo.Size()))
	}
	buffer := make([]byte, copyBufferSize)
	var read int64
	for {
		if err := checkContext(ctx); err != nil {
			return nil, false, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			read += int64(n)
			if maxBytes >= 0 && read > maxBytes {
				return nil, false, corruptf("%q exceeds its size limit", name)
			}
			_, _ = result.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, false, readErr
		}
	}
	return result.Bytes(), true, nil
}

func (s *Store) atomicWrite(ctx context.Context, parent, target string, data []byte) (retErr error) {
	temp, err := temporaryName(parent)
	if err != nil {
		return err
	}
	file, err := s.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); retErr == nil && closeErr != nil {
				retErr = closeErr
			}
		}
		if removeErr := s.root.Remove(temp); retErr == nil &&
			removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			retErr = removeErr
		}
	}()

	for offset := 0; offset < len(data); {
		if err := checkContext(ctx); err != nil {
			return err
		}
		end := min(offset+copyBufferSize, len(data))
		n, err := file.Write(data[offset:end])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		offset += n
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := s.root.Rename(temp, target); err != nil {
		return err
	}
	if err := s.syncDir(parent); err != nil {
		return err
	}
	return nil
}

func (s *Store) syncDir(name string) error {
	directory, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateID(id string) error {
	if len(id) != sha256.Size*2 {
		return fmt.Errorf("%w: expected 64 lowercase hexadecimal characters", ErrInvalidID)
	}
	for _, character := range id {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return fmt.Errorf("%w: expected 64 lowercase hexadecimal characters", ErrInvalidID)
		}
	}
	return nil
}

func digestBytes(ctx context.Context, data []byte) (string, error) {
	hash := sha256.New()
	for offset := 0; offset < len(data); {
		if err := checkContext(ctx); err != nil {
			return "", err
		}
		end := min(offset+copyBufferSize, len(data))
		_, _ = hash.Write(data[offset:end])
		offset = end
	}
	return hex.EncodeToString(hash.Sum(nil)), checkContext(ctx)
}

func objectParent(id string) string {
	return objectsDir + "/" + id[:2]
}

func objectPath(id string) string {
	return objectParent(id) + "/" + id[2:]
}

func referenceParent(id string) string {
	return referencesDir + "/" + id[:2]
}

func referencePath(id string) string {
	return referenceParent(id) + "/" + id[2:] + ".ref"
}

func temporaryName(parent string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate temporary name: %w", err)
	}
	return parent + "/.tmp-" + hex.EncodeToString(random[:]), nil
}

func corruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, arguments...))
}

func tamperedf(format string, arguments ...any) error {
	return errors.Join(ErrCorrupt, fmt.Errorf("%w: %s", ErrDigestMismatch, fmt.Sprintf(format, arguments...)))
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	return ctx.Err()
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

var _ interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	Retain(context.Context, string) error
	Release(context.Context, string) error
	Delete(context.Context, string) error
	Close(context.Context) error
} = (*Store)(nil)
