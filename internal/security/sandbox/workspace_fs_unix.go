//go:build !windows

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func (w *Workspace) OpenFile(name string) (*os.File, error) {
	parent, base, err := w.openParent(name, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open workspace file %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), base))
	if err := w.validateOpened(file, false); err != nil {
		file.Close()
		return nil, fmt.Errorf("open workspace file %q: %w", name, err)
	}
	return file, nil
}

func (w *Workspace) OpenDirectory(name string) (*os.File, error) {
	if name == "" || name == "." {
		return w.openRoot()
	}
	parts, err := w.relativeParts(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		// Absolute (or cleaned) path naming the workspace root.
		return w.openRoot()
	}
	parent, base, err := w.openParent(name, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(
		int(parent.Fd()), base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open workspace directory %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), base))
	if err := w.validateOpened(file, true); err != nil {
		file.Close()
		return nil, fmt.Errorf("open workspace directory %q: %w", name, err)
	}
	return file, nil
}

func (w *Workspace) AtomicWrite(name string, data []byte, mode fs.FileMode) error {
	parent, base, err := w.openParent(name, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	if existing, openErr := unix.Openat(
		int(parent.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	); openErr == nil {
		file := os.NewFile(uintptr(existing), base)
		validateErr := w.validateOpened(file, false)
		closeErr := file.Close()
		if validateErr != nil {
			return validateErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(openErr, unix.ENOENT) {
		return fmt.Errorf("validate workspace target %q: %w", name, openErr)
	}

	var temporary string
	var file *os.File
	for range 32 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return err
		}
		temporary = ".qcode-write-" + hex.EncodeToString(suffix)
		fd, openErr := unix.Openat(
			int(parent.Fd()), temporary,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0o600,
		)
		if openErr == nil {
			file = os.NewFile(uintptr(fd), filepath.Join(parent.Name(), temporary))
			break
		}
		if !errors.Is(openErr, unix.EEXIST) {
			return openErr
		}
	}
	if file == nil {
		return errors.New("create workspace temporary file: name attempts exhausted")
	}
	defer unix.Unlinkat(int(parent.Fd()), temporary, 0)
	if err := file.Chmod(mode.Perm()); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), base); err != nil {
		return fmt.Errorf("commit workspace file %q: %w", name, err)
	}
	return parent.Sync()
}

// AtomicCreate creates a file only if it is still absent at the final directory
// descriptor. It is the no-clobber companion to AtomicWrite.
func (w *Workspace) AtomicCreate(name string, data []byte, mode fs.FileMode) error {
	parent, base, err := w.openParent(name, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := unix.Openat(
		int(parent.Fd()), base,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		uint32(mode.Perm()),
	)
	if err != nil {
		return fmt.Errorf("create workspace file %q without clobber: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), base))
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = unix.Unlinkat(int(parent.Fd()), base, 0)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = unix.Unlinkat(int(parent.Fd()), base, 0)
		return err
	}
	if err := file.Close(); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), base, 0)
		return err
	}
	return parent.Sync()
}

// Remove unlinks a regular workspace file at the final directory descriptor. It
// applies the same target validation as AtomicWrite, so a symlink or a file
// outside the workspace device is refused rather than followed.
func (w *Workspace) Remove(name string) error {
	parent, base, err := w.openParent(name, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open workspace file %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), base))
	validateErr := w.validateOpened(file, false)
	closeErr := file.Close()
	if validateErr != nil {
		return fmt.Errorf("remove workspace file %q: %w", name, validateErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := unix.Unlinkat(int(parent.Fd()), base, 0); err != nil {
		return fmt.Errorf("remove workspace file %q: %w", name, err)
	}
	return parent.Sync()
}

func (w *Workspace) openParent(name string, createParents bool) (*os.File, string, error) {
	parts, err := w.relativeParts(name)
	if err != nil {
		return nil, "", err
	}
	if len(parts) == 0 {
		return nil, "", errors.New("workspace file path cannot name the root")
	}
	current, err := w.openRoot()
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		exact, exactErr := exactEntryNameFile(current, part)
		if exactErr != nil {
			if !createParents || !errors.Is(exactErr, os.ErrNotExist) {
				current.Close()
				return nil, "", exactErr
			}
			if err := unix.Mkdirat(int(current.Fd()), part, 0o755); err != nil &&
				!errors.Is(err, unix.EEXIST) {
				current.Close()
				return nil, "", err
			}
			exact = part
		}
		fd, openErr := unix.Openat(
			int(current.Fd()), exact,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY,
			0,
		)
		childName := filepath.Join(current.Name(), exact)
		current.Close()
		if openErr != nil {
			return nil, "", fmt.Errorf("open workspace parent %q: %w", part, openErr)
		}
		current = os.NewFile(uintptr(fd), childName)
		if err := w.validateOpened(current, true); err != nil {
			current.Close()
			return nil, "", err
		}
	}
	return current, parts[len(parts)-1], nil
}

func (w *Workspace) openRoot() (*os.File, error) {
	fd, err := unix.Open(w.root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	file := os.NewFile(uintptr(fd), w.root)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	identity, err := identityOf(w.root, info)
	if err != nil {
		file.Close()
		return nil, err
	}
	if identity.device != w.identity.device || identity.inode != w.identity.inode {
		file.Close()
		return nil, errors.New("workspace root identity changed")
	}
	return file, nil
}

func (w *Workspace) validateOpened(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return errors.New("workspace object has an unexpected type")
	}
	identity, err := identityOf("", info)
	if err != nil {
		return err
	}
	if identity.device != w.identity.device {
		return errors.New("workspace object crosses a device boundary")
	}
	if !directory && identity.links > 1 {
		return ErrMultiplyLinked
	}
	return nil
}

func (w *Workspace) relativeParts(name string) ([]string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return nil, errors.New("workspace path is required and cannot contain NUL")
	}
	target := name
	if !filepath.IsAbs(target) {
		target = filepath.Join(w.root, target)
	}
	relative, err := filepath.Rel(w.root, filepath.Clean(target))
	if err != nil || outside(relative) {
		return nil, fmt.Errorf("path %q is outside workspace", name)
	}
	if relative == "." {
		return nil, nil
	}
	return strings.Split(relative, string(filepath.Separator)), nil
}

func exactEntryNameFile(directory *os.File, requested string) (string, error) {
	duplicate, err := unix.Dup(int(directory.Fd()))
	if err != nil {
		return "", err
	}
	copy := os.NewFile(uintptr(duplicate), directory.Name())
	defer copy.Close()
	entries, err := copy.ReadDir(-1)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == requested {
			return requested, nil
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), requested) {
			return "", fmt.Errorf("path component %q has incorrect case", requested)
		}
	}
	return "", os.ErrNotExist
}
