package keyring

import (
	"context"
	"fmt"

	oskeyring "github.com/zalando/go-keyring"
)

const defaultService = "qcode"

var ErrNotFound = oskeyring.ErrNotFound

// Store is the macOS Keychain credential store via zalando/go-keyring.
type Store struct {
	Service string
}

func New() Store {
	return Store{Service: defaultService}
}

func (s Store) service() string {
	if s.Service != "" {
		return s.Service
	}
	return defaultService
}

func (s Store) Lookup(_ context.Context, name string) (string, error) {
	value, err := oskeyring.Get(s.service(), name)
	if err != nil {
		return "", fmt.Errorf("keyring lookup: %w", err)
	}
	return value, nil
}

// Set writes a secret into the OS keyring under the given user name.
func (s Store) Set(name, secret string) error {
	if name == "" {
		return fmt.Errorf("keyring name is required")
	}
	if secret == "" {
		return fmt.Errorf("keyring secret is empty")
	}
	return oskeyring.Set(s.service(), name, secret)
}

// Delete removes a secret; missing entries are ignored.
func (s Store) Delete(name string) error {
	if name == "" {
		return nil
	}
	err := oskeyring.Delete(s.service(), name)
	if err == nil || err == oskeyring.ErrNotFound {
		return nil
	}
	return err
}
