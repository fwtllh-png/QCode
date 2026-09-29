package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestChromeArgumentsRouteEveryRequestThroughTheProxy(t *testing.T) {
	arguments := strings.Join(chromeArguments("/tmp/profile", "http://127.0.0.1:4321"), "\n")
	for _, want := range []string{
		"--proxy-server=http://127.0.0.1:4321",
		"--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--user-data-dir=/tmp/profile",
		"--remote-debugging-pipe",
	} {
		if !strings.Contains(arguments, want) {
			t.Fatalf("chrome arguments missing %q:\n%s", want, arguments)
		}
	}
	if strings.Contains(arguments, "--remote-debugging-port") {
		t.Fatal("browser debugger is exposed to local clients")
	}
}

func TestChromeTrafficCannotReachUngrantedHostServices(t *testing.T) {
	binary := findChromeBinary()
	if binary == "" {
		t.Skip("Chrome is not installed")
	}
	var secretHits atomic.Int32
	secret := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		secretHits.Add(1)
		_, _ = writer.Write([]byte("secret"))
	}))
	defer secret.Close()
	secretURL, err := url.Parse(secret.URL)
	if err != nil {
		t.Fatal(err)
	}
	secretPort := secretURL.Port()

	page := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, secret.URL+"/redirected", http.StatusFound)
			return
		}
		writer.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(writer, `<!doctype html><html><body>
<p id="marker">approved page</p>
<img src="http://127.0.0.1:%[1]s/pixel">
<img src="http://localhost:%[1]s/named">
<iframe src="http://127.0.0.1:%[1]s/frame"></iframe>
<script>fetch("http://127.0.0.1:%[1]s/fetch", {mode: "no-cors"}).catch(() => {});</script>
</body></html>`, secretPort)
	}))
	defer page.Close()
	pageURL, err := url.Parse(page.URL)
	if err != nil {
		t.Fatal(err)
	}
	pagePort, err := strconv.Atoi(pageURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	browser := newChromeBrowser(binary).(*chromeBrowser)
	defer browser.Close()
	ctx, closeScope := egress.WithScope(t.Context())
	defer closeScope()
	egress.AllowInScope(ctx, egress.Target{
		Host: "127.0.0.1", Protocol: "http", Port: uint16(pagePort), AllowPrivate: true,
	})
	snapshot, err := browser.Navigate(ctx, page.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, "approved page") {
		t.Fatalf("approved loopback page did not load through the proxy:\n%s", snapshot)
	}
	if _, err := os.Stat(filepath.Join(browser.profile, "DevToolsActivePort")); !os.IsNotExist(err) {
		t.Fatal("browser exposed a debugger TCP endpoint")
	}
	for _, argument := range browser.command.Args {
		if strings.Contains(argument, browser.proxy.Credential()) {
			t.Fatal("proxy credential leaked into Chrome argv")
		}
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", browser.proxy.Port()))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	foreign := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := foreign.Get(page.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("foreign browser proxy status = %d", response.StatusCode)
	}
	if _, err := browser.Navigate(ctx, page.URL+"/redirect"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if hits := secretHits.Load(); hits != 0 {
		t.Fatalf("page reached an ungranted host service %d times", hits)
	}
}
