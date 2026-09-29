package credential_test

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	securitycredential "github.com/fwtllh-png/QCode/internal/security/credential"
	oskeyring "github.com/zalando/go-keyring"
)

func TestStoreRoundTripViaMock(t *testing.T) {
	oskeyring.MockInit()
	store := securitycredential.NewKeyringStore()
	const name = "provider/test-key"
	const secret = "mock-keyring-secret"
	if err := store.Set(name, secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(name) })

	got, err := store.Lookup(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("Lookup = %q, want %q", got, secret)
	}

	resolver := httpclient.Credentials{Keyring: store}
	value, err := resolver.Resolve(context.Background(), model.CredentialRef{
		Kind: "keyring", Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if value != secret {
		t.Fatalf("Resolve = %q", value)
	}
}

func TestDefaultCredentialsWiresSystemKeyring(t *testing.T) {
	creds := httpclient.DefaultCredentials()
	if creds.Keyring == nil {
		t.Fatal("DefaultCredentials().Keyring is nil")
	}
}
