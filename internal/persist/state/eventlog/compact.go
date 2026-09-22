package eventlog

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Compact atomically replaces the log while keeping global sequence numbers.
// The caller durably records the removal set before calling and repairs its
// offset projection afterwards; retrying the same set is safe after a crash.
// Optional archives are gzip JSONL named by the digest of their uncompressed
// bytes. They are fsynced before the source log is replaced.
func (l *Log) Compact(ctx context.Context, remove map[protocol.Cursor]bool, archiveDir string) (int, error) {
	l.readers.Lock()
	defer l.readers.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	temp, err := os.CreateTemp(filepath.Dir(l.path), ".events-compact-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		if temp != nil {
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		}
	}()
	next := &Log{path: l.path, file: temp}
	var archive *os.File
	var compressed *gzip.Writer
	digest := sha256.New()
	if archiveDir != "" {
		if err := os.MkdirAll(archiveDir, 0o700); err != nil {
			return 0, err
		}
		archive, err = os.CreateTemp(archiveDir, ".events-archive-*")
		if err != nil {
			return 0, err
		}
		defer func() {
			_ = archive.Close()
			_ = os.Remove(archive.Name())
		}()
		compressed = gzip.NewWriter(archive)
	}
	removed := 0
	for _, evidence := range l.entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		record, err := l.readVerifiedRecord(evidence)
		if err != nil {
			return 0, err
		}
		raw, err := json.Marshal(record.Event)
		if err != nil {
			return 0, err
		}
		raw = append(raw, '\n')
		if remove[evidence.Sequence] {
			if compressed != nil {
				if err := writeFull(io.MultiWriter(compressed, digest), raw); err != nil {
					return 0, err
				}
			}
			removed++
			continue
		}
		if err := writeFull(temp, raw); err != nil {
			return 0, err
		}
		next.indexOwner(record.Event, len(next.entries))
		next.entries = append(next.entries, makeEvidence(evidence.Sequence, next.end, raw))
		next.end += int64(len(raw))
		next.last = evidence.Sequence
	}
	if compressed != nil {
		if err := errors.Join(compressed.Close(), archive.Sync()); err != nil {
			return 0, err
		}
		if removed > 0 {
			name := hex.EncodeToString(digest.Sum(nil)) + ".jsonl.gz"
			if err := os.Rename(archive.Name(), filepath.Join(archiveDir, name)); err != nil {
				return 0, err
			}
			if err := syncDirectory(archiveDir); err != nil {
				return 0, err
			}
		}
	}
	if err := temp.Sync(); err != nil {
		return 0, err
	}
	if err := os.Rename(temp.Name(), l.path); err != nil {
		return 0, err
	}
	old := l.file
	// Transfer the open descriptor before reporting a directory-sync error.
	// Once renamed, future appends must use the new inode.
	l.file = temp
	l.entries, l.bySession, l.byThread = next.entries, next.bySession, next.byThread
	l.last, l.end = next.last, next.end
	// Prevent the temporary-file defer from closing the transferred handle.
	temp = nil
	return removed, errors.Join(old.Close(), syncDirectory(filepath.Dir(l.path)))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
