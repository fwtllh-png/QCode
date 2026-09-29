package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

// chromeBrowser owns one headless Chrome whose every request crosses a
// Runtime-owned loopback proxy. The proxy gate admits public destinations
// and only the host-network targets that approved web_run calls granted.
type chromeBrowser struct {
	binary string

	mu         sync.Mutex
	gate       *egress.Gate
	proxy      *egress.ManagedNetworkProxy
	command    *exec.Cmd
	profile    string
	connection *websocket.Conn
	nextID     int64
}

type cdpMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func newChromeBrowser(binary string) BrowserRuntime {
	return &chromeBrowser{binary: binary, gate: newBrowserGate()}
}

func newBrowserGate() *egress.Gate {
	return &egress.Gate{AllowPublic: true}
}

// chromeArguments pins Chrome to the proxy: loopback is not implicitly
// bypassed, local name resolution fails so nothing can connect around the
// proxy, and WebRTC may not send non-proxied UDP.
func chromeArguments(profile, proxyURL string) []string {
	return []string{
		"--headless=new",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--metrics-recording-only",
		"--no-default-browser-check",
		"--no-first-run",
		"--proxy-server=" + proxyURL,
		"--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"about:blank",
	}
}

func findChromeBinary() string {
	if configured := strings.TrimSpace(os.Getenv("QCODE_BROWSER_BINARY")); configured != "" {
		if filepath.IsAbs(configured) {
			if info, err := os.Stat(configured); err == nil && !info.IsDir() {
				return configured
			}
			return ""
		}
		if path, err := exec.LookPath(configured); err == nil {
			return path
		}
		return ""
	}
	for _, candidate := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	for _, candidate := range []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge",
	} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

func (b *chromeBrowser) ensureStarted(ctx context.Context) error {
	if b.connection != nil {
		return nil
	}
	proxy, err := egress.StartUnauthenticatedNetworkProxy(b.gate)
	if err != nil {
		return fmt.Errorf("start browser egress proxy: %w", err)
	}
	b.proxy = proxy
	profile, err := os.MkdirTemp("", "qcode-browser-")
	if err != nil {
		_ = b.closeLocked()
		return err
	}
	b.profile = profile
	command := exec.Command(b.binary, chromeArguments(profile, proxy.URL())...)
	if err := command.Start(); err != nil {
		_ = b.closeLocked()
		return err
	}
	b.command = command

	portFile := filepath.Join(profile, "DevToolsActivePort")
	var port int
	for {
		data, readErr := os.ReadFile(portFile)
		if readErr == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 {
				port, err = strconv.Atoi(strings.TrimSpace(lines[0]))
				if err == nil && port > 0 {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			_ = b.closeLocked()
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/new?about%%3Ablank", port)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		_ = b.closeLocked()
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		_ = b.closeLocked()
		return err
	}
	defer response.Body.Close()
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&target); err != nil {
		_ = b.closeLocked()
		return err
	}
	if target.WebSocketDebuggerURL == "" {
		_ = b.closeLocked()
		return errors.New("Chrome did not return a page debugger endpoint")
	}
	connection, _, err := websocket.Dial(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		_ = b.closeLocked()
		return err
	}
	connection.SetReadLimit(32 << 20)
	b.connection = connection
	if _, err := b.call(ctx, "Page.enable", nil); err != nil {
		_ = b.closeLocked()
		return err
	}
	if _, err := b.call(ctx, "Runtime.enable", nil); err != nil {
		_ = b.closeLocked()
		return err
	}
	return nil
}

func (b *chromeBrowser) call(
	ctx context.Context,
	method string,
	params any,
) (json.RawMessage, error) {
	b.nextID++
	id := b.nextID
	if err := wsjson.Write(ctx, b.connection, map[string]any{
		"id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}
	for {
		var message cdpMessage
		if err := wsjson.Read(ctx, b.connection, &message); err != nil {
			return nil, err
		}
		if message.ID != id {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("CDP %s: %s", method, message.Error.Message)
		}
		return message.Result, nil
	}
}

func (b *chromeBrowser) evaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	result, err := b.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return nil, err
	}
	var evaluated struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(result, &evaluated); err != nil {
		return nil, err
	}
	if len(evaluated.Exception) > 0 && string(evaluated.Exception) != "null" {
		return nil, errors.New("browser JavaScript evaluation failed")
	}
	return evaluated.Result.Value, nil
}

func (b *chromeBrowser) Navigate(ctx context.Context, rawURL string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptCallGrants(ctx)
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.Host == "" {
		return "", errors.New("browser URL must use http or https without credentials")
	}
	if err := b.ensureStarted(ctx); err != nil {
		return "", err
	}
	if _, err := b.call(ctx, "Page.navigate", map[string]any{"url": parsed.String()}); err != nil {
		return "", err
	}
	for {
		value, err := b.evaluate(ctx, "document.readyState")
		if err != nil {
			return "", err
		}
		var state string
		if json.Unmarshal(value, &state) == nil && state == "complete" {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return b.snapshotLocked(ctx)
}

func (b *chromeBrowser) Snapshot(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptCallGrants(ctx)
	if err := b.ensureStarted(ctx); err != nil {
		return "", err
	}
	return b.snapshotLocked(ctx)
}

func (b *chromeBrowser) snapshotLocked(ctx context.Context) (string, error) {
	value, err := b.evaluate(ctx, "document.documentElement ? document.documentElement.outerHTML : ''")
	if err != nil {
		return "", err
	}
	var snapshot string
	if err := json.Unmarshal(value, &snapshot); err != nil {
		return "", err
	}
	return snapshot, nil
}

func (b *chromeBrowser) Click(ctx context.Context, selector string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptCallGrants(ctx)
	if err := b.ensureStarted(ctx); err != nil {
		return err
	}
	encoded, _ := json.Marshal(selector)
	value, err := b.evaluate(ctx, `(selector => {
		const node = document.querySelector(selector);
		if (!node) return false;
		node.click();
		return true;
	})(`+string(encoded)+`)`)
	if err != nil {
		return err
	}
	var clicked bool
	if json.Unmarshal(value, &clicked) != nil || !clicked {
		return errors.New("browser selector did not match an element")
	}
	return nil
}

func (b *chromeBrowser) Fill(ctx context.Context, selector, input string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptCallGrants(ctx)
	if err := b.ensureStarted(ctx); err != nil {
		return err
	}
	encodedSelector, _ := json.Marshal(selector)
	encodedValue, _ := json.Marshal(input)
	value, err := b.evaluate(ctx, `(args => {
		const node = document.querySelector(args[0]);
		if (!node) return false;
		node.focus();
		node.value = args[1];
		node.setAttribute('value', args[1]);
		node.dispatchEvent(new Event('input', {bubbles: true}));
		node.dispatchEvent(new Event('change', {bubbles: true}));
		return true;
	})([`+string(encodedSelector)+`,`+string(encodedValue)+`])`)
	if err != nil {
		return err
	}
	var filled bool
	if json.Unmarshal(value, &filled) != nil || !filled {
		return errors.New("browser selector did not match a fillable element")
	}
	return nil
}

func (b *chromeBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeLocked()
}

// adoptCallGrants keeps the targets Guard approved for this web_run call for
// the rest of the browser session: pages keep loading from an approved
// intranet or localhost origin after the call returns.
func (b *chromeBrowser) adoptCallGrants(ctx context.Context) {
	if b.gate == nil {
		b.gate = newBrowserGate()
	}
	b.gate.AdoptScope(ctx)
}

// closeLocked ends the browser session, including every adopted grant.
func (b *chromeBrowser) closeLocked() error {
	var result error
	if b.connection != nil {
		result = b.connection.Close(websocket.StatusNormalClosure, "closing")
		b.connection = nil
	}
	if b.command != nil && b.command.Process != nil {
		if err := b.command.Process.Kill(); err != nil && result == nil {
			result = err
		}
		_ = b.command.Wait()
		b.command = nil
	}
	if b.proxy != nil {
		if err := b.proxy.Close(context.Background()); err != nil && result == nil {
			result = err
		}
		b.proxy = nil
	}
	if b.profile != "" {
		if err := os.RemoveAll(b.profile); err != nil && result == nil {
			result = err
		}
		b.profile = ""
	}
	b.gate = newBrowserGate()
	return result
}
