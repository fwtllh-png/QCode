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
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestGateDeniesUntilGranted(t *testing.T) {
	gate := egress.NewStaticGate()
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

func TestNilGateDeniesEveryRequest(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(server.Close)
	var gate *egress.Gate
	resp, err := egress.WrapClient(&http.Client{}, gate).Get(server.URL)
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, egress.ErrDenied) || hits.Load() != 0 {
		t.Fatalf("nil gate: error=%v hits=%d, want denied before connecting", err, hits.Load())
	}
	if gate.Allowed("127.0.0.1", "http") {
		t.Fatal("nil gate reported a host as allowed")
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if err := gate.Check(request); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("nil gate Check() = %v, want ErrDenied", err)
	}
}

func TestGateRefusesTransportItCannotPin(t *testing.T) {
	gate := egress.NewStaticGate()
	gate.Allow("origin.test", "https")
	called := false
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	resp, err := (&http.Client{Transport: gate.RoundTripper(base)}).Get("https://origin.test/")
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, egress.ErrDenied) || called {
		t.Fatalf("error=%v called=%t, want refusal before the base transport runs", err, called)
	}
}

func TestGatePinsResolvedAddressOverBaseDialer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pinned")
	}))
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	}
	gate.AllowURL("http://pinned.test:" + port)
	baseDialed := false
	base := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			baseDialed = true
			return nil, errors.New("base dialer must not run")
		},
	}
	resp, err := (&http.Client{Transport: gate.RoundTripper(base)}).Get("http://pinned.test:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pinned" || baseDialed {
		t.Fatalf("body=%q baseDialed=%t", body, baseDialed)
	}
}

func TestGateCancellationClosesRequestWithBody(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(server.Close)
	gate := egress.NewStaticGate()
	gate.AllowURL(server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{}`))
	resp, err := egress.WrapClient(&http.Client{}, gate).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		server.CloseClientConnections()
		t.Fatal("canceling the request left the upstream connection open")
	}
}

func TestGateReleasesPinnedConnectionAfterBodyClose(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	gate := egress.NewStaticGate()
	gate.AllowURL(server.URL)
	resp, err := egress.WrapClient(&http.Client{}, gate).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("pinned per-request connection stayed pooled after the body closed")
	}
}

type pinnedRecorder struct{ addresses []net.IP }

func (*pinnedRecorder) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unpinned RoundTrip must not run")
}

func (r *pinnedRecorder) RoundTripPinned(req *http.Request, addresses []net.IP) (*http.Response, error) {
	r.addresses = addresses
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestGateHandsApprovedAddressesToPinnedTransport(t *testing.T) {
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	gate.Allow("origin.test", "https")
	base := &pinnedRecorder{}
	resp, err := (&http.Client{Transport: gate.RoundTripper(base)}).Get("https://origin.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(base.addresses) != 1 || !base.addresses[0].Equal(net.ParseIP("93.184.216.34")) {
		t.Fatalf("pinned addresses = %v", base.addresses)
	}
}

func TestRedirectToUngrantedHostIsDenied(t *testing.T) {
	var sawOther atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sawOther.Store(true)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/path", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	gate := egress.NewStaticGate()
	gate.AllowURL(origin.URL)
	resp, err := egress.WrapClient(&http.Client{}, gate).Get(origin.URL + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected redirect target to be denied")
	}
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if sawOther.Load() {
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
					ip := net.ParseIP("93.184.216.34")
					expected := test.public
					if private {
						ip = net.ParseIP("10.0.0.1")
						expected = test.private
					}
					gate := &egress.Gate{
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
	gate := egress.NewStaticGate()
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
	gate := egress.NewStaticGate()
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
	gate := egress.NewStaticGate()
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

func TestGateConstructorsFixWhatTheGateConsults(t *testing.T) {
	public := func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	target := egress.Target{Host: "example.com", Protocol: "https", Methods: []string{"GET"}}

	static := egress.NewStaticGate(target)
	static.LookupIP = public
	if _, err := static.Authorize(context.Background(), target, "test"); err != nil {
		t.Fatalf("static gate denied its grant: %v", err)
	}
	other := egress.Target{Host: "other.example", Protocol: "https", Methods: []string{"GET"}}
	if _, err := static.Authorize(context.Background(), other, "test"); err == nil {
		t.Fatal("static gate allowed an ungranted public target")
	}

	browser := egress.NewBrowserGate()
	browser.LookupIP = public
	if _, err := browser.Authorize(context.Background(), other, "test"); err != nil {
		t.Fatalf("browser gate denied a public target: %v", err)
	}

	scoped := egress.NewCallScopedGate()
	scoped.LookupIP = public
	if _, err := scoped.Authorize(context.Background(), other, "test"); err == nil {
		t.Fatal("call-scoped gate allowed an ungranted target outside a call")
	}
}
