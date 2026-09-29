package credential

import (
	"context"
	"fmt"

	oskeyring "github.com/zalando/go-keyring"
)

const defaultKeyringService = "qcode"

var ErrKeyringNotFound = oskeyring.ErrNotFound

// KeyringStore is the macOS Keychain credential store via zalando/go-keyring.
type KeyringStore struct {
	Service string
}

func NewKeyringStore() KeyringStore {
	return KeyringStore{Service: defaultKeyringService}
}

func (s KeyringStore) service() string {
	if s.Service != "" {
		return s.Service
	}
	return defaultKeyringService
}

func (s KeyringStore) Lookup(_ context.Context, name string) (string, error) {
	value, err := oskeyring.Get(s.service(), name)
	if err != nil {
		return "", fmt.Errorf("keyring lookup: %w", err)
	}
	return value, nil
}

// Set writes a secret into the OS keyring under the given user name.
func (s KeyringStore) Set(name, secret string) error {
	if name == "" {
		return fmt.Errorf("keyring name is required")
	}
	if secret == "" {
		return fmt.Errorf("keyring secret is empty")
	}
	return oskeyring.Set(s.service(), name, secret)
}

// Delete removes a secret; missing entries are ignored.
func (s KeyringStore) Delete(name string) error {
	if name == "" {
		return nil
	}
	err := oskeyring.Delete(s.service(), name)
	if err == nil || err == oskeyring.ErrNotFound {
		return nil
	}
	return err
}
