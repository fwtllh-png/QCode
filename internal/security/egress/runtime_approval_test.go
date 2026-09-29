package egress_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestAuthorizeProbeDoesNotAskApprover(t *testing.T) {
	gate := &egress.Gate{}
	gate.SetRuntimeApprover(func(context.Context, egress.Target) error {
		t.Fatal("probe Authorize must not start an approval")
		return nil
	})
	_, err := gate.Authorize(t.Context(), egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}, "test")
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("probe error = %v", err)
	}
}

func TestAuthorizeAsksApproverBeforeResolve(t *testing.T) {
	var resolved atomic.Int32
	var asked atomic.Int32
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			resolved.Add(1)
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	gate.SetRuntimeApprover(func(_ context.Context, target egress.Target) error {
		if resolved.Load() != 0 {
			t.Fatal("resolver ran before approval")
		}
		asked.Add(1)
		if target.Host != "cdn.example" || target.Protocol != "https" {
			t.Fatalf("target = %+v", target)
		}
		return nil
	})
	ips, err := gate.AuthorizeBeforeConnect(t.Context(), egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}, "test")
	if err != nil || len(ips) != 1 || asked.Load() != 1 || resolved.Load() != 1 {
		t.Fatalf("ips=%v asked=%d resolved=%d err=%v", ips, asked.Load(), resolved.Load(), err)
	}
}

func TestAuthorizeSettledDenialDoesNotAskAgain(t *testing.T) {
	var asked atomic.Int32
	var resolved atomic.Int32
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			resolved.Add(1)
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	gate.SetRuntimeApprover(func(context.Context, egress.Target) error {
		asked.Add(1)
		return errors.New("rejected")
	})
	target := egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}
	_, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test")
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("first error = %v", err)
	}
	denied, ok := egress.DeniedTarget(err)
	if !ok || !denied.ApprovalSettled {
		t.Fatalf("settled = %+v ok=%t", denied, ok)
	}
	_, err = gate.AuthorizeBeforeConnect(t.Context(), target, "test")
	if !errors.Is(err, egress.ErrDenied) || asked.Load() != 1 || resolved.Load() != 0 {
		t.Fatalf("second ask=%d resolve=%d err=%v", asked.Load(), resolved.Load(), err)
	}
}

func TestAuthorizeSharesOneApprovalAcrossWaiters(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var asked atomic.Int32
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	gate.SetRuntimeApprover(func(context.Context, egress.Target) error {
		if asked.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	})
	target := egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test")
			done <- err
		}()
	}
	<-started
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("asked = %d", asked.Load())
	}
}

func TestProcessSessionApprovesConnectBeforeDial(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)

	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	session, err := proxy.OpenSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	var asked atomic.Int32
	session.Gate().SetRuntimeApprover(func(_ context.Context, target egress.Target) error {
		asked.Add(1)
		if target.Host != httpTarget(t, upstream.URL, http.MethodGet).Host {
			t.Fatalf("target = %+v", target)
		}
		return nil
	})

	assertProxyBody(t, session, upstream.URL, http.StatusOK, "ok")
	if asked.Load() != 1 {
		t.Fatalf("asked = %d", asked.Load())
	}
	if _, err := workspace.Authorize(
		t.Context(),
		httpTarget(t, upstream.URL, http.MethodGet),
		"test",
	); err == nil {
		t.Fatal("runtime grant leaked onto the workspace gate")
	}
}

func TestBoundRuntimeApproverEndsWithItsCall(t *testing.T) {
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	waiting := make(chan struct{})
	var asked atomic.Int32
	release := gate.BindRuntimeApprover(func(ctx context.Context, _ egress.Target) error {
		asked.Add(1)
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	})
	inFlight := egress.Target{
		Host: "late.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}
	result := make(chan error, 1)
	go func() {
		_, err := gate.AuthorizeBeforeConnect(context.Background(), inFlight, "test")
		result <- err
	}()
	<-waiting
	release()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("an approval wait that outlived its call was granted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the call did not cancel the in-flight approval wait")
	}

	after := egress.Target{
		Host: "background.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}
	if _, err := gate.AuthorizeBeforeConnect(context.Background(), after, "test"); err == nil {
		t.Fatal("a target discovered after the call ended was granted")
	}
	if asked.Load() != 1 {
		t.Fatalf("asked = %d, want only the in-call request", asked.Load())
	}
}

func TestOverlappingRuntimeApproverReleaseKeepsTheNewerCall(t *testing.T) {
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	first := gate.BindRuntimeApprover(func(context.Context, egress.Target) error {
		return errors.New("stale approver answered")
	})
	second := gate.BindRuntimeApprover(func(context.Context, egress.Target) error { return nil })
	defer second()
	first()
	target := egress.Target{
		Host: "current.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test"); err != nil {
		t.Fatalf("newer call lost its approver: %v", err)
	}
}

func TestApprovalDecisionIsMethodScoped(t *testing.T) {
	var asked atomic.Int32
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	gate.SetRuntimeApprover(func(_ context.Context, target egress.Target) error {
		asked.Add(1)
		return nil
	})
	get := egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}
	connect := get
	connect.Methods = []string{http.MethodConnect}
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), get, "test"); err != nil {
		t.Fatal(err)
	}
	// Same method replays from the settled decision without re-asking.
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), get, "test"); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("GET asked %d times, settled decisions must replay", asked.Load())
	}
	// A GET approval must never answer a CONNECT on the same origin.
	if _, err := gate.AuthorizeBeforeConnect(t.Context(), connect, "test"); err != nil {
		t.Fatalf("CONNECT re-ask failed: %v", err)
	}
	if asked.Load() != 2 {
		t.Fatalf("CONNECT reused the GET approval: asked=%d", asked.Load())
	}
}

func TestApprovedPublicOriginCannotRebindToPrivate(t *testing.T) {
	var lookups atomic.Int32
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			// Public at approval time, private afterwards: a DNS rebinding.
			if lookups.Add(1) == 1 {
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			}
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		},
	}
	gate.SetRuntimeApprover(func(context.Context, egress.Target) error { return nil })
	target := egress.Target{
		Host: "cdn.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}
	if ips, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test"); err != nil || len(ips) != 1 {
		t.Fatalf("first authorize ips=%v err=%v", ips, err)
	}
	_, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test")
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("rebound private resolution was allowed: %v", err)
	}
	denied, ok := egress.DeniedTarget(err)
	if !ok || !strings.Contains(denied.Reason, "private") {
		t.Fatalf("denied = %+v ok=%t", denied, ok)
	}
}

func TestApprovedPrivateTargetGrantsPrivateDialing(t *testing.T) {
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		},
	}
	gate.SetRuntimeApprover(func(context.Context, egress.Target) error { return nil })
	target := egress.Target{
		Host: "goproxy.corp.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodGet},
	}
	for attempt := 0; attempt < 2; attempt++ {
		ips, err := gate.AuthorizeBeforeConnect(t.Context(), target, "test")
		if err != nil || len(ips) != 1 {
			t.Fatalf("attempt %d ips=%v err=%v", attempt, ips, err)
		}
	}
}
