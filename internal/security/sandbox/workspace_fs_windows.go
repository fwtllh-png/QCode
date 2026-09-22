//go:build windows

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
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

// fileDispositionInformation is FILE_DISPOSITION_INFORMATION: a single BOOLEAN
// that marks the handle's file for deletion when the last handle closes.
type fileDispositionInformation struct {
	DeleteFile uint8
}

func (w *Workspace) OpenFile(name string) (*os.File, error) {
	parent, base, err := w.openParentHandle(name, false, false)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeHandle(
		parent, base, windows.FILE_GENERIC_READ, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
	)
	if err != nil {
		return nil, fmt.Errorf("open workspace file %q: %w", name, err)
	}
	if err := w.validateHandle(handle, false); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), filepath.Join(w.root, name)), nil
}

func (w *Workspace) OpenDirectory(name string) (*os.File, error) {
	if name == "" || name == "." {
		handle, err := w.openRootHandle(false)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), w.root), nil
	}
	parts, err := w.relativePartsWindows(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		handle, err := w.openRootHandle(false)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), w.root), nil
	}
	parent, base, err := w.openParentHandle(name, false, false)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeHandle(
		parent, base, windows.FILE_GENERIC_READ, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE,
	)
	if err != nil {
		return nil, fmt.Errorf("open workspace directory %q: %w", name, err)
	}
	if err := w.validateHandle(handle, true); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), filepath.Join(w.root, name)), nil
}

func (w *Workspace) AtomicWrite(name string, data []byte, mode fs.FileMode) error {
	parent, base, err := w.openParentHandle(name, true, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)

	var temporary string
	var handle windows.Handle
	for range 32 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return err
		}
		temporary = ".qcode-write-" + hex.EncodeToString(suffix)
		handle, err = openRelativeHandle(
			parent, temporary,
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_WRITE_THROUGH,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return err
		}
	}
	if handle == 0 {
		return errors.New("create workspace temporary file: name attempts exhausted")
	}
	file := os.NewFile(uintptr(handle), filepath.Join(w.root, temporary))
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
	if err := renameHandle(handle, parent, base); err != nil {
		file.Close()
		return fmt.Errorf("commit workspace file %q: %w", name, err)
	}
	return file.Close()
}

func (w *Workspace) AtomicCreate(name string, data []byte, mode fs.FileMode) error {
	parent, base, err := w.openParentHandle(name, true, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeHandle(
		parent, base,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_WRITE_THROUGH,
	)
	if err != nil {
		return fmt.Errorf("create workspace file %q without clobber: %w", name, err)
	}
	file := os.NewFile(uintptr(handle), filepath.Join(w.root, name))
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
	return file.Close()
}

// Remove deletes a regular workspace file through a descriptor-relative handle,
// applying the same target validation as AtomicWrite.
func (w *Workspace) Remove(name string) error {
	parent, base, err := w.openParentHandle(name, false, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeHandle(
		parent, base, windows.FILE_GENERIC_READ|windows.DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
	)
	if err != nil {
		return fmt.Errorf("open workspace file %q: %w", name, err)
	}
	defer windows.CloseHandle(handle)
	if err := w.validateHandle(handle, false); err != nil {
		return fmt.Errorf("remove workspace file %q: %w", name, err)
	}
	disposition := fileDispositionInformation{DeleteFile: 1}
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(
		handle, &status, (*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)), windows.FileDispositionInformation,
	); err != nil {
		return fmt.Errorf("remove workspace file %q: %w", name, err)
	}
	return nil
}

func (w *Workspace) openParentHandle(
	name string,
	createParents bool,
	writable bool,
) (windows.Handle, string, error) {
	parts, err := w.relativePartsWindows(name)
	if err != nil {
		return 0, "", err
	}
	if len(parts) == 0 {
		return 0, "", errors.New("workspace file path cannot name the root")
	}
	current, err := w.openRootHandle(writable)
	if err != nil {
		return 0, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		exact, exactErr := exactEntryNameHandle(current, part)
		if exactErr != nil {
			if !createParents || !errors.Is(exactErr, os.ErrNotExist) {
				windows.CloseHandle(current)
				return 0, "", exactErr
			}
			exact = part
		}
		access := uint32(windows.FILE_GENERIC_READ)
		if writable {
			access |= windows.FILE_GENERIC_WRITE | windows.DELETE
		}
		disposition := uint32(windows.FILE_OPEN)
		if createParents {
			disposition = windows.FILE_OPEN_IF
		}
		next, openErr := openRelativeHandle(
			current, exact, access, disposition, windows.FILE_DIRECTORY_FILE,
		)
		windows.CloseHandle(current)
		if openErr != nil {
			return 0, "", fmt.Errorf("open workspace parent %q: %w", part, openErr)
		}
		current = next
		if err := w.validateHandle(current, true); err != nil {
			windows.CloseHandle(current)
			return 0, "", err
		}
	}
	return current, parts[len(parts)-1], nil
}

func (w *Workspace) openRootHandle(writable bool) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(w.root)
	if err != nil {
		return 0, err
	}
	access := uint32(windows.GENERIC_READ)
	if writable {
		access |= windows.GENERIC_WRITE | windows.DELETE
	}
	handle, err := windows.CreateFile(
		name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open workspace root: %w", err)
	}
	if err := w.validateHandle(handle, true); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func (w *Workspace) validateHandle(handle windows.Handle, directory bool) error {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("workspace object is a reparse point")
	}
	isDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("workspace object has an unexpected type")
	}
	if uint64(information.VolumeSerialNumber) != w.identity.device {
		return errors.New("workspace object crosses a volume boundary")
	}
	if !directory && information.NumberOfLinks > 1 {
		return ErrMultiplyLinked
	}
	return nil
}

func openRelativeHandle(
	parent windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	options uint32,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle, access, &attributes, &status, nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	return handle, err
}

func exactEntryNameHandle(directory windows.Handle, requested string) (string, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		process, directory, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(duplicate), "workspace-directory")
	defer file.Close()
	entries, err := file.ReadDir(-1)
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

func renameHandle(handle, parent windows.Handle, name string) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	nameBytes := (len(encoded) - 1) * 2
	var layout fileRenameInformation
	size := int(unsafe.Offsetof(layout.FileName)) + nameBytes
	buffer := make([]byte, size)
	information := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS |
		windows.FILE_RENAME_POSIX_SEMANTICS
	information.RootDirectory = parent
	information.FileNameLength = uint32(nameBytes)
	target := (*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&information.FileName[0]))
	copy(target[:nameBytes/2:nameBytes/2], encoded[:len(encoded)-1])
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		handle, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation,
	)
}

func (w *Workspace) relativePartsWindows(name string) ([]string, error) {
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
