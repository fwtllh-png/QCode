package egress_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestGateDeniesUntilGranted(t *testing.T) {
	gate := &egress.Gate{Enforce: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	client := egress.WrapClient(&http.Client{}, gate)
	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected egress denied before grant")
	}
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if !strings.Contains(err.Error(), "egress denied") {
		t.Fatalf("error = %q, want stable egress denied text", err)
	}
	target, ok := egress.DeniedTarget(err)
	if !ok || target.Host != "127.0.0.1" || target.Protocol != "http" ||
		target.Port == 0 || target.Method != http.MethodGet {
		t.Fatalf("DeniedTarget() = %+v, %t", target, ok)
	}

	if !gate.AllowURL(server.URL) {
		t.Fatal("AllowURL failed")
	}
	resp, err = client.Get(server.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestGateOpenWhenNotEnforcing(t *testing.T) {
	gate := &egress.Gate{Enforce: false}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	client := egress.WrapClient(&http.Client{}, gate)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestNilGateLeavesClientOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	client := egress.WrapClient(&http.Client{}, nil)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestRedirectToUngrantedHostIsDenied(t *testing.T) {
	gate := &egress.Gate{Enforce: true}
	gate.Allow("origin.test", "https")

	var sawOther bool
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "origin.test":
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://other.test/path"}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		case "other.test":
			sawOther = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       http.NoBody,
				Request:    req,
			}, nil
		default:
			t.Fatalf("unexpected host %q", req.URL.Hostname())
			return nil, nil
		}
	})
	client := &http.Client{Transport: gate.RoundTripper(base)}
	resp, err := client.Get("https://origin.test/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected redirect target to be denied")
	}
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if sawOther {
		t.Fatal("RoundTrip reached the ungranted host")
	}
}

func TestGateAccumulatesMethodAndPrivateGrants(t *testing.T) {
	tests := []struct {
		name    string
		grants  []egress.Target
		public  map[string]bool
		private map[string]bool
	}{
		{
			name: "separate methods",
			grants: []egress.Target{
				{Methods: []string{" get ", "GET"}},
				{Methods: []string{"POST"}, AllowPrivate: true},
			},
			public:  map[string]bool{"GET": true, "POST": true},
			private: map[string]bool{"POST": true},
		},
		{
			name: "same method keeps private permission",
			grants: []egress.Target{
				{Methods: []string{"GET"}, AllowPrivate: true},
				{Methods: []string{"GET"}},
			},
			public:  map[string]bool{"GET": true},
			private: map[string]bool{"GET": true},
		},
		{
			name: "all public methods plus private POST",
			grants: []egress.Target{
				{},
				{Methods: []string{"POST"}, AllowPrivate: true},
			},
			public:  map[string]bool{"GET": true, "POST": true, "DELETE": true, "CONNECT": true},
			private: map[string]bool{"POST": true},
		},
		{
			name: "all methods stay granted after CONNECT",
			grants: []egress.Target{
				{AllowPrivate: true},
				{Methods: []string{"CONNECT"}},
			},
			public:  map[string]bool{"GET": true, "POST": true, "DELETE": true, "CONNECT": true},
			private: map[string]bool{"GET": true, "POST": true, "DELETE": true, "CONNECT": true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				for _, private := range []bool{false, true} {
					ip := net.ParseIP("203.0.113.1")
					expected := test.public
					if private {
						ip = net.ParseIP("10.0.0.1")
						expected = test.private
					}
					gate := &egress.Gate{
						Enforce: true,
						LookupIP: func(context.Context, string) ([]net.IP, error) {
							return []net.IP{ip}, nil
						},
					}
					for index := range test.grants {
						if reverse {
							index = len(test.grants) - 1 - index
						}
						grant := test.grants[index]
						grant.Host, grant.Protocol = "API.Example.", "HTTPS"
						gate.AllowTarget(grant)
						gate.AllowTarget(grant) // Repeated approvals are idempotent.
					}
					for _, methods := range [][]string{
						{"GET"}, {"POST"}, {"DELETE"}, {"CONNECT"}, {"GET", "POST"},
					} {
						want := true
						for _, method := range methods {
							want = want && expected[method]
						}
						_, err := gate.Authorize(t.Context(), egress.Target{
							Host: "api.example", Protocol: "https", Methods: methods,
						}, "test")
						if (err == nil) != want || (err != nil && !errors.Is(err, egress.ErrDenied)) {
							t.Errorf("reverse=%t private=%t methods=%v: error=%v, want allowed=%t",
								reverse, private, methods, err, want)
						}
					}
				}
			}
		})
	}
}

func TestGateGrantDoesNotEscapeOriginOrOmitMethod(t *testing.T) {
	gate := &egress.Gate{Enforce: true}
	methods := []string{"GET"}
	gate.AllowTarget(egress.Target{
		Host: "127.0.0.1", Protocol: "http", Port: 8080,
		Methods: methods, AllowPrivate: true,
	})
	methods[0] = "DELETE"
	for _, request := range []egress.Target{
		{Host: "127.0.0.1", Protocol: "http", Port: 8080, Methods: []string{"DELETE"}},
		{Host: "127.0.0.1", Protocol: "http", Port: 8081, Methods: []string{"GET"}},
		{Host: "127.0.0.1", Protocol: "https", Port: 8080, Methods: []string{"GET"}},
		{Host: "127.0.0.2", Protocol: "http", Port: 8080, Methods: []string{"GET"}},
		{Host: "127.0.0.1", Protocol: "http", Port: 8080},
	} {
		if _, err := gate.Authorize(t.Context(), request, "test"); !errors.Is(err, egress.ErrDenied) {
			t.Errorf("request=%+v, error=%v, want denied", request, err)
		}
	}
	// A malformed later grant cannot revoke or extend the original permission.
	gate.AllowTarget(egress.Target{
		Host: "127.0.0.1", Protocol: "http", Port: 8080,
		Methods: []string{"POST\r\nGET"}, AllowPrivate: true,
	})
	if _, err := gate.Authorize(t.Context(), egress.Target{
		Host: "127.0.0.1", Protocol: "http", Port: 8080, Methods: []string{"GET"},
	}, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestGateConcurrentGrantsPreserveExistingAccess(t *testing.T) {
	gate := &egress.Gate{Enforce: true}
	target := egress.Target{Host: "127.0.0.1", Protocol: "http", AllowPrivate: true}
	target.Methods = []string{"GET"}
	gate.AllowTarget(target)
	var workers sync.WaitGroup
	for _, method := range []string{"POST", "PUT", "PATCH", "HEAD", "OPTIONS"} {
		workers.Go(func() {
			grant := target
			grant.Methods = []string{method}
			gate.AllowTarget(grant)
			for _, request := range []egress.Target{target, grant} {
				if _, err := gate.Authorize(t.Context(), request, "test"); err != nil {
					t.Errorf("concurrent grant revoked %v: %v", request.Methods, err)
				}
			}
		})
	}
	workers.Wait()
	target.Methods = []string{"GET", "POST", "PUT", "PATCH", "HEAD", "OPTIONS"}
	if _, err := gate.Authorize(t.Context(), target, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeDeniedReceiptCarriesEnvironmentCategory(t *testing.T) {
	gate := &egress.Gate{Enforce: true}
	_, err := gate.Authorize(t.Context(), egress.Target{
		Host: "code.byted.org", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}, "process_proxy")
	if err == nil || !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("Authorize() error = %v", err)
	}
	denied, ok := egress.DeniedTarget(err)
	if !ok || denied.Category != "network_target_unapproved" ||
		denied.RequiredAction != "approve_network_target" {
		t.Fatalf("DeniedTarget() = %+v, %t", denied, ok)
	}
	receipts := gate.Receipts()
	if len(receipts) != 1 || receipts[0].Decision != "deny" ||
		receipts[0].Category != "network_target_unapproved" ||
		receipts[0].RequiredAction != "approve_network_target" ||
		receipts[0].Host != "code.byted.org" {
		t.Fatalf("receipts = %+v", receipts)
	}

	gate = &egress.Gate{
		Enforce: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return nil, errors.New("nxdomain")
		},
	}
	gate.AllowTarget(egress.Target{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	})
	_, err = gate.Authorize(t.Context(), egress.Target{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}, "process_proxy")
	if err == nil || !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("DNS Authorize() error = %v", err)
	}
	denied, ok = egress.DeniedTarget(err)
	if !ok || denied.Category != "" || denied.Reason != "DNS resolution failed" {
		t.Fatalf("DNS DeniedTarget() = %+v, %t", denied, ok)
	}
	receipts = gate.Receipts()
	if len(receipts) != 1 || receipts[0].Category != "" {
		t.Fatalf("DNS receipts must not invent a missing-capability category: %+v", receipts)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
