package guard

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

// An approved URL carries private reach only as its host resolves at grant
// time, so a public origin cannot later rebind onto private addresses.
func TestURLGrantPrivateReachFollowsGrantTimeResolution(t *testing.T) {
	grantTime := map[string]string{
		"docs.example.com":  "93.184.216.34",
		"wiki.corp.example": "10.1.2.3",
	}
	guard := &Guard{lookupIP: func(_ context.Context, host string) ([]net.IP, error) {
		address, ok := grantTime[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return []net.IP{net.ParseIP(address)}, nil
	}}
	dialTime := "10.9.9.9"
	gate := egress.NewCallScopedGate()
	gate.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(dialTime)}, nil
	}
	authorize := func(rawURL string) error {
		t.Helper()
		ctx, closeScope := egress.WithScope(t.Context())
		defer closeScope()
		guard.grantNetworkHosts(ctx, resolvePolicyFixture(policyInvocationFixture{
			Capability: tool.CapabilityNetwork,
			Resources: []tool.Resource{{
				Kind: "url", ID: rawURL, Access: tool.AccessRead,
				Methods: []string{"GET"},
			}},
		}))
		target, err := netpolicy.ParseTarget(rawURL)
		if err != nil {
			t.Fatalf("parse %s: %v", rawURL, err)
		}
		_, err = gate.Authorize(ctx, egress.Target{
			Host: target.Host, Protocol: target.Scheme, Port: target.Port,
			Methods: []string{"GET"},
		}, "web")
		return err
	}

	if err := authorize("https://docs.example.com/guide"); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("public origin rebinding to private err = %v", err)
	}
	if err := authorize("https://wiki.corp.example/page"); err != nil {
		t.Fatalf("intranet origin denied: %v", err)
	}
	dialTime = "127.0.0.1"
	if err := authorize("http://127.0.0.1:3000/"); err != nil {
		t.Fatalf("explicit loopback URL denied: %v", err)
	}
}
