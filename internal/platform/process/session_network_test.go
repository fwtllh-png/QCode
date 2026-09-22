//go:build darwin

package process

import (
	"io"
	"testing"
)

func TestCreateAppliesSessionProxyPortAndClosesNetwork(t *testing.T) {
	manager := NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	root := t.TempDir()
	backend := &recordingBackend{root: root, proxyPort: 43128}
	closer := &closeRecorder{}
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "true", Dir: root, ThreadID: testSessionThread,
		Sandbox: backend, SessionProxyPort: 43129, Network: closer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.command.SessionProxyPort != 43129 {
		t.Fatalf("session port binding = %+v", backend.command)
	}
	if environmentValue(backend.command.Env, "HTTPS_PROXY") != "http://127.0.0.1:43129" {
		t.Fatalf("session proxy environment = %v", backend.command.Env)
	}
	if closer.closed {
		t.Fatal("network closed before session teardown")
	}
	if err := manager.Close(id, testSessionThread); err != nil {
		t.Fatal(err)
	}
	if !closer.closed {
		t.Fatal("session close did not recycle the network channel")
	}
}

func TestCreateRollbackClosesUnregisteredNetwork(t *testing.T) {
	manager := NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	closer := &closeRecorder{}
	_, err := manager.Create(t.Context(), SessionOptions{
		Command: "", Dir: "", ThreadID: testSessionThread,
		SessionProxyPort: 43129, Network: closer,
	})
	if err == nil {
		t.Fatal("Create() succeeded with an empty directory")
	}
	if !closer.closed {
		t.Fatal("failed Create left the network channel open")
	}
}

type closeRecorder struct{ closed bool }

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

var _ io.Closer = (*closeRecorder)(nil)
