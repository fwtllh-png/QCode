package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	securitycredential "github.com/fwtllh-png/QCode/internal/security/credential"
)

const maxCredentialBytes = 64 << 10

type Keyring interface {
	Lookup(context.Context, string) (string, error)
}

type Credentials struct {
	LookupEnv func(string) (string, bool)
	SecretDir string
	Keyring   Keyring
}

func DefaultCredentials() Credentials {
	secretDir := os.Getenv("QCODE_SECRET_DIR")
	if secretDir == "" {
		if configDir, err := os.UserConfigDir(); err == nil {
			secretDir = filepath.Join(configDir, "qcode", "secrets")
		}
	}
	return Credentials{
		LookupEnv: os.LookupEnv,
		SecretDir: secretDir,
		Keyring:   securitycredential.NewKeyringStore(),
	}
}

func (c Credentials) Resolve(ctx context.Context, reference model.CredentialRef) (string, error) {
	if reference.Kind == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch reference.Kind {
	case "env":
		lookup := c.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, exists := lookup(reference.Name)
		if !exists {
			return "", fmt.Errorf("credential environment variable %s is not set", reference.Name)
		}
		return validateCredential(value, "environment")
	case "file":
		return c.resolveFile(reference.Name)
	case "keyring":
		if c.Keyring == nil {
			return "", errors.New("credential keyring is not configured")
		}
		value, err := c.Keyring.Lookup(ctx, reference.Name)
		if err != nil {
			return "", fmt.Errorf("credential keyring entry %s is unavailable", reference.Name)
		}
		return validateCredential(value, "keyring")
	default:
		return "", fmt.Errorf("credential kind %q is not available", reference.Kind)
	}
}

func (c Credentials) resolveFile(name string) (string, error) {
	if c.SecretDir == "" {
		return "", errors.New("credential secret directory is not configured")
	}
	root, err := filepath.Abs(c.SecretDir)
	if err != nil {
		return "", errors.New("credential secret directory is invalid")
	}
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(name)))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("credential file is outside the secret directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("credential file %s is unavailable", name)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("credential file %s is not a regular file", name)
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return "", fmt.Errorf("credential file %s must be owner-readable and inaccessible to group and others", name)
	}
	if !credentialOwnedByCurrentUser(info) {
		return "", fmt.Errorf("credential file %s is not owned by the current user", name)
	}

	file, err := openCredentialFile(path)
	if err != nil {
		return "", fmt.Errorf("credential file %s cannot be opened", name)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", fmt.Errorf("credential file %s changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return "", fmt.Errorf("credential file %s cannot be read", name)
	}
	if len(data) > maxCredentialBytes {
		return "", fmt.Errorf("credential file %s exceeds %d bytes", name, maxCredentialBytes)
	}
	return validateCredential(strings.TrimRight(string(data), "\r\n"), "file")
}

func validateCredential(value, source string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("credential %s value is empty", source)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("credential %s value contains invalid data", source)
	}
	return value, nil
}

var _ CredentialResolver = Credentials{}
