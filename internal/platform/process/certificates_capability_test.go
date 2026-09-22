//go:build capability && darwin

package process_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestSandboxNodeUsesValidatedCertificateDependencies(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "public-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", caPath)
	target, _ := url.Parse(server.URL)
	port, _ := strconv.ParseUint(target.Port(), 10, 16)
	gate := &egress.Gate{Enforce: true}
	gate.AllowTarget(egress.Target{
		Host: target.Hostname(), Protocol: "https", Port: uint16(port),
		Methods: []string{"CONNECT"}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close(context.Background())
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: t.TempDir(), ManagedProxyPort: proxy.Port(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("sandbox_unavailable: %v", err)
	}
	policy, _ := sandbox.BackendPolicy(backend)
	pinned, err := process.OpenPinnedDirectory(backend, policy.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	// Read all discovered files, then use a real CONNECT tunnel and verify TLS.
	// Neither the client nor the proxy disables certificate verification.
	files, _ := json.Marshal(policy.Toolchains.ReadFiles)
	script := `const fs=require('fs'),http=require('http'),tls=require('tls');
for(const file of JSON.parse(process.argv[1])) fs.readFileSync(file);
try { fs.writeFileSync(process.env.NODE_EXTRA_CA_CERTS,'modified'); process.exit(90); }
catch(e) { if(e.code!=='EPERM'&&e.code!=='EACCES') throw e; }
const target=new URL(process.argv[2]),proxy=new URL(process.env.HTTPS_PROXY);
const req=http.request({hostname:proxy.hostname,port:proxy.port,method:'CONNECT',path:target.host});
req.on('connect',(res,socket)=>{
 if(res.statusCode!==200) process.exit(91);
 const secure=tls.connect({socket,servername:'example.com'},()=>{
  if(!secure.authorized) process.exit(92);
  secure.end();console.log('verified');
 });
 secure.on('error',e=>{console.error(e.code);process.exitCode=1});
});req.on('error',e=>{console.error(e.code);process.exitCode=1});req.end();`
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Path: node, Args: []string{"-e", script, string(files), server.URL},
		Dir: policy.WorkspaceRoot, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil || result.ExitCode != 0 || result.Stdout != "verified\n" {
		t.Fatalf("sandbox TLS verification: %+v %v", result, err)
	}
}
