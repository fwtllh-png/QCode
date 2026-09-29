package egress_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestPublicBrowsingGateAdmitsOnlyPublicDestinations(t *testing.T) {
	resolved := map[string][]string{
		"cdn.example":      {"93.184.216.34"},
		"mixed.example":    {"93.184.216.34", "10.1.2.3"},
		"rebind.example":   {"127.0.0.1"},
		"metadata.example": {"169.254.169.254"},
		"wiki.corp":        {"10.1.2.3"},
		"localhost":        {"127.0.0.1"},
	}
	gate := &egress.Gate{
		AllowPublic: true,
		LookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			addresses, ok := resolved[host]
			if !ok {
				return nil, errors.New("no such host")
			}
			return fixedLookup(addresses...)(context.Background(), host)
		},
	}
	connect := func(host string, port uint16) egress.Target {
		return egress.Target{
			Host: host, Protocol: "https", Port: port, Methods: []string{"CONNECT"},
		}
	}
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), connect("cdn.example", 443), "browser"); err != nil {
		t.Fatalf("public destination denied: %v", err)
	}
	for _, target := range []egress.Target{
		connect("mixed.example", 443),
		connect("rebind.example", 443),
		connect("metadata.example", 80),
		connect("wiki.corp", 443),
		connect("127.0.0.1", 6732),
		connect("169.254.169.254", 80),
		connect("missing.example", 443),
		{Host: "localhost", Protocol: "http", Port: 3000, Methods: []string{"GET"}},
	} {
		if _, err := gate.AuthorizeBeforeConnect(t.Context(), target, "browser"); !errors.Is(err, egress.ErrDenied) {
			t.Fatalf("non-public destination %s:%d err = %v", target.Host, target.Port, err)
		}
	}

	gate.AllowTarget(egress.Target{
		Host: "wiki.corp", Protocol: "https", Port: 443, AllowPrivate: true,
	})
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), connect("wiki.corp", 443), "browser"); err != nil {
		t.Fatalf("granted intranet destination denied: %v", err)
	}
	resolved["wiki.corp"] = []string{"127.0.0.1"}
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), connect("wiki.corp", 443), "browser"); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("granted intranet name rebinding to loopback err = %v", err)
	}
	gate.AllowTarget(egress.Target{
		Host: "localhost", Protocol: "http", Port: 3000, AllowPrivate: true,
	})
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), egress.Target{
		Host: "localhost", Protocol: "http", Port: 3000, Methods: []string{"GET"},
	}, "browser"); err != nil {
		t.Fatalf("granted localhost origin denied: %v", err)
	}
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), egress.Target{
		Host: "localhost", Protocol: "http", Port: 3001, Methods: []string{"GET"},
	}, "browser"); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("ungranted localhost port err = %v", err)
	}
}

func TestAdoptScopeKeepsApprovedTargetsBeyondTheCall(t *testing.T) {
	browser := &egress.Gate{AllowPublic: true, LookupIP: fixedLookup("10.1.2.3")}
	target := egress.Target{
		Host: "wiki.corp", Protocol: "https", Port: 443, Methods: []string{"CONNECT"},
	}
	browser.AdoptScope(t.Context())
	if _, err := browser.Authorize(t.Context(), target, "browser"); err == nil {
		t.Fatal("adopting without a call scope granted a target")
	}
	ctx, closeScope := egress.WithScope(t.Context())
	egress.AllowInScope(ctx, egress.Target{
		Host: "wiki.corp", Protocol: "https", Port: 443, AllowPrivate: true,
	})
	browser.AdoptScope(ctx)
	closeScope()
	if _, err := browser.Authorize(context.Background(), target, "browser"); err != nil {
		t.Fatalf("approved target did not outlive its call: %v", err)
	}
	other := target
	other.Host = "other.corp"
	if _, err := browser.Authorize(context.Background(), other, "browser"); err == nil {
		t.Fatal("adoption widened past the approved target")
	}
}
