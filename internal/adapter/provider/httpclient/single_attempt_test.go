package httpclient

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
)

func TestSingleAttemptDoesNotFollowRedirectsOrRetryHTTPFailures(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308, 429, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirected" {
					writeGuardianSSE(w)
					return
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := testClient()
			request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
			request.SingleAttempt = true
			stream, err := client.Stream(t.Context(), request)
			if stream != nil {
				_ = stream.Close()
			}
			if err == nil || calls.Load() != 1 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func writeGuardianSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

// A pooled fixture transport for an already numeric, approved loopback host.
// It implements the gate's pinning contract without DNS resolution.
type guardianPinnedPool struct{ base *http.Transport }

func (p *guardianPinnedPool) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("pinning required")
}
func (p *guardianPinnedPool) RoundTripPinned(r *http.Request, ips []net.IP) (*http.Response, error) {
	host := net.ParseIP(r.URL.Hostname())
	if host == nil || !host.IsLoopback() || len(ips) != 1 || !host.Equal(ips[0]) {
		return nil, errors.New("fixture target was not pinned")
	}
	return p.base.RoundTrip(r)
}

// The server drops a reused connection after reading the second request.
// net/http normally replays this idempotent POST; SingleAttempt must forbid it.
func TestSingleAttemptDisablesStaleConnectionReplay(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(map[bool]string{false: "control_replays", true: "single_attempt"}[single], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if calls.Add(1) == 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				writeGuardianSSE(w)
			}))
			defer server.Close()
			client := testClient()
			pool := &guardianPinnedPool{base: client.HTTP.Transport.(*http.Transport)}
			client.HTTP.Transport = pool
			defer pool.base.CloseIdleConnections()
			warm, err := client.httpClient().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, warm.Body)
			_ = warm.Body.Close()
			request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
			request.SingleAttempt = single
			stream, err := client.Stream(t.Context(), request)
			if stream != nil {
				_, _ = provider.Drain(stream)
				_ = stream.Close()
			}
			if single {
				if err == nil || calls.Load() != 2 {
					t.Fatalf("single attempt: calls=%d err=%v", calls.Load(), err)
				}
			} else if err != nil || calls.Load() != 3 {
				t.Fatalf("replay control: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestClientThroughputReservationsShareOneAtomicController(t *testing.T) {
	client := testClient()
	route := testRequest(t, "http://127.0.0.1:1", model.ProtocolOpenAIChat).Route
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for range 2 {
		wg.Go(func() {
			if client.TryReserveThroughput(route, 75, 100).Status == providerratelimit.StatusAdmit {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 || client.DecideThroughput(route, 26, 100).Status == providerratelimit.StatusAdmit {
		t.Fatal("concurrent samples bypassed throughput reservation")
	}
}
