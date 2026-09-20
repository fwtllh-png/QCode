package egress_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestManagedProxyForwardsOnlyGrantedHTTPMethod(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		_, _ = io.WriteString(writer, request.Method+" ok")
	}))
	t.Cleanup(upstream.Close)
	targetURL, _ := url.Parse(upstream.URL)
	portValue, _ := strconv.ParseUint(targetURL.Port(), 10, 16)
	port := uint16(portValue)

	gate := &egress.Gate{Enforce: true}
	gate.AllowTarget(egress.Target{
		Host: targetURL.Hostname(), Protocol: "http", Port: port,
		Methods: []string{http.MethodGet}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	t.Cleanup(client.CloseIdleConnections)

	response, err := client.Get(upstream.URL + "/allowed")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "GET ok" {
		t.Fatalf("body = %q", body)
	}
	request, _ := http.NewRequest(http.MethodPost, upstream.URL, nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("POST status = %d", response.StatusCode)
	}
	gate.AllowTarget(egress.Target{
		Host: targetURL.Hostname(), Protocol: "http", Port: port,
		Methods: []string{http.MethodPost}, AllowPrivate: true,
	})
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		request, _ := http.NewRequest(method, upstream.URL, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodDelete {
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("unapproved DELETE status = %d", response.StatusCode)
			}
		} else if response.StatusCode != http.StatusOK || string(body) != method+" ok" {
			t.Fatalf("after POST grant: %s status=%d body=%q", method, response.StatusCode, body)
		}
	}
}

func TestManagedProxyCONNECTUsesApprovedResolvedAddress(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "secure")
	}))
	t.Cleanup(upstream.Close)
	targetURL, _ := url.Parse(upstream.URL)
	_, rawPort, _ := net.SplitHostPort(targetURL.Host)
	port, _ := strconv.ParseUint(rawPort, 10, 16)
	gate := &egress.Gate{Enforce: true}
	gate.AllowTarget(egress.Target{
		Host: targetURL.Hostname(), Protocol: "https", Port: uint16(port),
		Methods: []string{http.MethodConnect}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // Test server certificate.
		},
	}}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "secure" {
		t.Fatalf("body = %q", body)
	}
}

func TestGateBlocksPrivateDNSResolutionWithoutExplicitGrant(t *testing.T) {
	gate := &egress.Gate{
		Enforce: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		},
	}
	gate.AllowTarget(egress.Target{
		Host: "metadata.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	})
	_, err := gate.Authorize(t.Context(), egress.Target{
		Host: "metadata.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}, "test")
	if err == nil || !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("Authorize() error = %v", err)
	}
	receipts := gate.Receipts()
	if len(receipts) != 1 || receipts[0].Decision != "deny" {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestManagedProxyRechecksRedirectTarget(t *testing.T) {
	var reached bool
	redirected := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		reached = true
	}))
	t.Cleanup(redirected.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		http.Redirect(writer, request, redirected.URL, http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	originURL, _ := url.Parse(origin.URL)
	port, _ := strconv.ParseUint(originURL.Port(), 10, 16)
	gate := &egress.Gate{Enforce: true}
	gate.AllowTarget(egress.Target{
		Host: originURL.Hostname(), Protocol: "http", Port: uint16(port),
		Methods: []string{http.MethodGet}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || reached {
		t.Fatalf("redirect status=%d reached=%t", response.StatusCode, reached)
	}
}
