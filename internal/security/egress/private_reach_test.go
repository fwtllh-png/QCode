package egress_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func fixedLookup(addresses ...string) func(context.Context, string) ([]net.IP, error) {
	return func(context.Context, string) ([]net.IP, error) {
		if len(addresses) == 0 {
			return nil, errors.New("no such host")
		}
		ips := make([]net.IP, 0, len(addresses))
		for _, address := range addresses {
			ips = append(ips, net.ParseIP(address))
		}
		return ips, nil
	}
}

func TestGrantPrivateFollowsGrantTimeResolution(t *testing.T) {
	for _, test := range []struct {
		name   string
		host   string
		lookup func(context.Context, string) ([]net.IP, error)
		want   bool
	}{
		{name: "loopback literal", host: "127.0.0.1", want: true},
		{name: "metadata literal", host: "169.254.169.254", want: true},
		{name: "intranet literal", host: "10.1.2.3", want: true},
		{name: "public literal", host: "93.184.216.34", want: false},
		{name: "localhost", host: "LocalHost.", want: true},
		{name: "intranet name", host: "wiki.corp.example", lookup: fixedLookup("10.1.2.3"), want: true},
		{name: "public name", host: "docs.example.com", lookup: fixedLookup("93.184.216.34"), want: false},
		{name: "name to loopback", host: "evil.example", lookup: fixedLookup("127.0.0.1"), want: false},
		{name: "name to metadata", host: "evil.example", lookup: fixedLookup("169.254.169.254"), want: false},
		{name: "mixed intranet and loopback", host: "evil.example", lookup: fixedLookup("10.1.2.3", "::1"), want: false},
		{name: "unresolvable name", host: "missing.example", lookup: fixedLookup(), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := egress.GrantPrivate(t.Context(), test.lookup, test.host); got != test.want {
				t.Fatalf("GrantPrivate(%q) = %t want %t", test.host, got, test.want)
			}
		})
	}
}

func TestWebScopeDeniesHostnamesRebindingToHostLocal(t *testing.T) {
	resolved := "10.1.2.3"
	gate := &egress.Gate{
		Enforce: true, UseCallScope: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP(resolved)}, nil
		},
	}
	ctx, closeScope := egress.WithScope(t.Context())
	defer closeScope()
	intranet := egress.Target{
		Host: "wiki.corp.example", Protocol: "https",
		Methods: []string{"GET"}, AllowPrivate: true,
	}
	egress.AllowInScope(ctx, intranet)
	if _, err := gate.Authorize(ctx, intranet, "web"); err != nil {
		t.Fatalf("intranet grant denied: %v", err)
	}
	for _, rebound := range []string{"127.0.0.1", "169.254.169.254", "::1"} {
		resolved = rebound
		if _, err := gate.Authorize(ctx, intranet, "web"); !errors.Is(err, egress.ErrDenied) {
			t.Fatalf("rebinding to %s err = %v", rebound, err)
		}
	}
	loopback := egress.Target{
		Host: "127.0.0.1", Protocol: "http", Port: 8080,
		Methods: []string{"GET"}, AllowPrivate: true,
	}
	egress.AllowInScope(ctx, loopback)
	if _, err := gate.Authorize(ctx, loopback, "web"); err != nil {
		t.Fatalf("explicit loopback grant denied: %v", err)
	}
}
