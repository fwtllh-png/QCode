package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type networkTransport func(*http.Request) (*http.Response, error)

func (f networkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func networkFixture(t *testing.T, rules []policy.Rule, base networkTransport) (*Guard, *egress.Gate) {
	t.Helper()
	gate := &egress.Gate{
		Enforce: true, UseCallScope: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	}
	registry := tool.NewRegistry(nil, nil)
	t.Cleanup(func() { _ = registry.Close() })
	if err := webtool.RegisterWithOptions(registry, webtool.Options{
		HTTP: egress.WrapClient(&http.Client{Transport: base}, gate),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	if _, err := runtime.ReloadSources(nil, rules); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		OnNetworkAllow: func(_ tool.Capability, target egress.Target) { gate.AllowTarget(target) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, gate
}

func networkResponse(r *http.Request, location string) *http.Response {
	response := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r,
	}
	if location != "" {
		response.StatusCode = http.StatusFound
		response.Header.Set("Location", location)
	}
	return response
}

func fetchNetwork(g *Guard, id, endpoint string) (tool.Result, error) {
	raw, _ := json.Marshal(map[string]string{"url": endpoint})
	return g.Execute(context.Background(), id, "web_fetch", raw)
}

func TestWebRedirectPortApproval(t *testing.T) {
	for _, port := range []uint16{443, 8443} {
		for _, decision := range []struct {
			approved bool
			scope    policy.ApprovalScope
		}{
			{true, policy.ApprovalOnce},
			{true, policy.ApprovalSession},
			{false, policy.ApprovalOnce},
		} {
			name := strconv.Itoa(int(port)) + "/" + string(decision.scope) + "/approved=" + strconv.FormatBool(decision.approved)
			t.Run(name, func(t *testing.T) {
				targetHits := 0
				var gate *egress.Gate
				var retainedContext context.Context
				g, transportGate := networkFixture(t, nil, func(r *http.Request) (*http.Response, error) {
					if r.URL.Hostname() == "source.example" {
						return networkResponse(r, "https://cdn.example:"+strconv.Itoa(int(port))+"/data"), nil
					}
					targetHits++
					retainedContext = r.Context()
					for _, unapproved := range []egress.Target{
						{Host: "cdn.example", Protocol: "https", Port: port + 1, Methods: []string{"GET"}},
						{Host: "cdn.example", Protocol: "https", Port: port, Methods: []string{"POST"}},
					} {
						if _, err := gate.Authorize(r.Context(), unapproved, "test"); !errors.Is(err, egress.ErrDenied) {
							t.Errorf("unapproved target=%+v: err=%v", unapproved, err)
						}
					}
					return networkResponse(r, ""), nil
				})
				gate = transportGate
				var redirect *NetworkApprovalContext
				g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
					allow := true
					if request.Network != nil && request.Network.Host == "cdn.example" {
						redirect = request.Network
						allow = decision.approved
					}
					return g.Decide(ApprovalDecision{
						RequestID: request.RequestID, Scope: decision.scope, Approved: allow,
					})
				})
				result, err := fetchNetwork(g, "redirect-port", "https://source.example/data")
				if err != nil || result.IsError == decision.approved {
					t.Errorf("approved=%t: err=%v result=%+v", decision.approved, err, result)
				}
				wantHits := 0
				if decision.approved {
					wantHits = 1
				}
				if targetHits != wantHits {
					t.Errorf("target hits=%d, want %d", targetHits, wantHits)
				}
				if redirect == nil || redirect.Port != port ||
					!slices.Equal(redirect.Methods, []string{"GET"}) {
					t.Errorf("redirect approval=%+v, want port=%d", redirect, port)
				}
				if retainedContext != nil {
					if _, err := gate.Authorize(retainedContext, egress.Target{
						Host: "cdn.example", Protocol: "https", Port: port, Methods: []string{"GET"},
					}, "after-call"); !errors.Is(err, egress.ErrDenied) {
						t.Errorf("completed redirect retained permission: %v", err)
					}
				}
			})
		}
	}
}

func TestHTTPRedirectApprovalUsesRedirectMethod(t *testing.T) {
	for _, scenario := range []struct {
		status int
		method string
	}{
		{http.StatusSeeOther, http.MethodGet},
		{http.StatusTemporaryRedirect, http.MethodPost},
	} {
		t.Run(scenario.method, func(t *testing.T) {
			targetHits := 0
			g, _ := networkFixture(t, nil, func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "source.example" {
					response := networkResponse(r, "http://cdn.example:8080/data")
					response.StatusCode = scenario.status
					return response, nil
				}
				targetHits++
				if r.Method != scenario.method {
					t.Errorf("redirect method=%s, want %s", r.Method, scenario.method)
				}
				return networkResponse(r, ""), nil
			})
			redirectApprovals := 0
			g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
				if request.Network != nil && request.Network.Host == "cdn.example" {
					redirectApprovals++
					if request.Network.Protocol != "http" || request.Network.Port != 8080 ||
						!slices.Equal(request.Network.Methods, []string{scenario.method}) {
						t.Errorf("redirect approval=%+v", request.Network)
					}
				}
				return g.Decide(ApprovalDecision{
					RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true,
				})
			})
			result, err := g.Execute(t.Context(), "redirect-method", "http_request",
				json.RawMessage(`{"url":"https://source.example/data","method":"POST","body":"fixture"}`))
			if err != nil || result.IsError || targetHits != 1 || redirectApprovals != 1 {
				t.Fatalf("err=%v result=%+v hits=%d approvals=%d", err, result, targetHits, redirectApprovals)
			}
		})
	}
}

func TestWebRedirectPortRepositoryDeny(t *testing.T) {
	targetHits, redirectApprovals := 0, 0
	g, _ := networkFixture(t, []policy.Rule{{
		Tool: "web_fetch", Resource: "https://cdn.example:8443/", Action: policy.ActionDeny,
	}}, func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "source.example" {
			return networkResponse(r, "https://cdn.example:8443/data"), nil
		}
		targetHits++
		return networkResponse(r, ""), nil
	})
	g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
		if request.Network != nil && request.Network.Host == "cdn.example" {
			redirectApprovals++
		}
		return g.Decide(ApprovalDecision{
			RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true,
		})
	})
	result, err := fetchNetwork(g, "redirect-denied-port", "https://source.example/data")
	var denied *policy.DecisionError
	switch {
	case errors.As(err, &denied) && denied.Code == "repository_rule_denied":
		// Guard-level denial before any fetch.
	case err == nil && result.IsError &&
		result.Metadata["error_category"] == "egress_denied" &&
		result.Metadata["host"] == "cdn.example":
		// Connect-time settled denial: the repository deny rule fires
		// inside the gated transport before a human is ever asked.
	default:
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if targetHits != 0 || redirectApprovals != 0 {
		t.Fatalf("target_hits=%d redirect_approvals=%d", targetHits, redirectApprovals)
	}
}

func TestWebUnknownEgressTargetNotApproved(t *testing.T) {
	hits, approvals := 0, 0
	g, _ := networkFixture(t, nil, func(*http.Request) (*http.Response, error) {
		hits++
		return nil, egress.ErrDenied
	})
	g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
		approvals++
		return g.Decide(ApprovalDecision{
			RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true,
		})
	})
	result, err := fetchNetwork(g, "unknown-target", "https://source.example/data")
	if err != nil || !result.IsError || result.Metadata["error_category"] != "egress_denied" ||
		hits != 1 || approvals != 1 {
		t.Fatalf("err=%v result=%+v hits=%d approvals=%d", err, result, hits, approvals)
	}
}

func TestStartupNetworkDeny(t *testing.T) {
	for _, resource := range []string{"denied.example", "localhost", "https://denied.example/data"} {
		for _, toolName := range []string{"web_fetch", "*"} {
			t.Run(toolName+"/"+resource, func(t *testing.T) {
				hits := 0
				g, _ := networkFixture(t, []policy.Rule{{
					Tool: toolName, Resource: resource, Action: policy.ActionDeny,
				}}, func(r *http.Request) (*http.Response, error) {
					hits++
					return networkResponse(r, ""), nil
				})
				g.policy.SetPermission(policy.PermissionAuto)
				g.policy.SetDisableAutoReview(false)
				endpoint := "https://denied.example/data"
				if resource == "localhost" {
					endpoint = "https://localhost/data"
				}
				_, err := fetchNetwork(g, "denied", endpoint)
				var denied *policy.DecisionError
				if !errors.As(err, &denied) || denied.Code != "repository_rule_denied" || hits != 0 {
					t.Fatalf("startup deny: err=%v transport_hits=%d", err, hits)
				}
			})
		}
	}
}

func TestWebRedirectPermissionIsolation(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		seed        bool
		approveSeed bool
		deny        bool
		fixed       bool
	}{
		{"cold", false, false, true, false},
		{"rejected_seed", true, false, true, false},
		{"approved_seed", true, true, true, false},
		{"allowed_redirect", true, true, false, false},
		{"fixed_target", false, false, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			grant, ok := policy.GrantForInvocation(policy.Invocation{
				Tool: "web_fetch", Capability: tool.CapabilityNetwork,
				Resources: []tool.Resource{{Kind: "url", ID: "https://denied.example/data", Access: tool.AccessRead}},
			})
			if !ok {
				t.Fatal("missing network grant")
			}
			var rules []policy.Rule
			if scenario.deny {
				rules = []policy.Rule{{Tool: "web_fetch", GrantKey: grant.Key, Action: policy.ActionDeny}}
			}
			hits := 0
			var retainedContext context.Context
			g, gate := networkFixture(t, rules, func(r *http.Request) (*http.Response, error) {
				retainedContext = r.Context()
				if r.URL.Hostname() == "source.example" {
					return networkResponse(r, "https://denied.example/data"), nil
				}
				hits++
				return networkResponse(r, ""), nil
			})
			if scenario.fixed {
				gate.Allow("denied.example", "https")
			}
			approvals := 0
			redirectApprovals := 0
			g.SetApprovalHandler(func(_ context.Context, request ApprovalRequest) error {
				approvals++
				if request.Tool == "web_fetch" && request.Network != nil && request.Network.Host == "denied.example" {
					redirectApprovals++
				}
				if approvals > 8 {
					return errors.New("unexpected approval loop")
				}
				return g.Decide(ApprovalDecision{
					RequestID: request.RequestID, Scope: policy.ApprovalOnce,
					Approved: request.Tool != "http_request" || scenario.approveSeed,
				})
			})
			if scenario.seed {
				result, err := g.Execute(t.Context(), "seed", "http_request",
					json.RawMessage(`{"url":"https://denied.example/seed","method":"GET"}`))
				if scenario.approveSeed && (err != nil || result.IsError || hits != 1) {
					t.Fatalf("approved seed: err=%v is_error=%t hits=%d", err, result.IsError, hits)
				}
				if !scenario.approveSeed {
					var denied *policy.DecisionError
					if !errors.As(err, &denied) || denied.Code != "approval_denied" || hits != 0 {
						t.Fatalf("rejected seed: err=%v hits=%d", err, hits)
					}
				}
			}
			if scenario.approveSeed {
				if gate.Allowed("denied.example", "https") {
					t.Fatal("dynamic permission leaked to shared gate")
				}
				if _, err := gate.Authorize(retainedContext, egress.Target{
					Host: "denied.example", Protocol: "https", Methods: []string{"GET"},
				}, "retained"); !errors.Is(err, egress.ErrDenied) {
					t.Fatalf("finished call retained permission: %v", err)
				}
			}
			before := hits
			result, err := fetchNetwork(g, "redirect", "https://source.example/data")
			if scenario.deny {
				var denied *policy.DecisionError
				if errors.As(err, &denied) && denied.Code == "repository_rule_denied" && hits == before {
					// Guard-level denial before any fetch.
				} else if err != nil || !result.IsError || hits != before {
					t.Fatalf("redirect deny: err=%v is_error=%t hits_delta=%d", err, result.IsError, hits-before)
				} else if result.Metadata["error_category"] != "egress_denied" ||
					result.Metadata["host"] != "denied.example" {
					// Connect-time settled denial: the redirect target fails
					// inside the gated transport with a structured egress
					// denial naming the host; the denied host is never
					// fetched.
					t.Fatalf("redirect deny metadata = %v", result.Metadata)
				}
			} else if err != nil || result.IsError || hits-before != 1 {
				t.Fatalf("approved redirect: err=%v is_error=%t hits_delta=%d", err, result.IsError, hits-before)
			}
			if !scenario.deny {
				result, err := fetchNetwork(g, "redirect-again", "https://source.example/data")
				if err != nil || result.IsError || redirectApprovals != 2 || hits-before != 2 {
					t.Fatalf("repeated redirect: err=%v is_error=%t approvals=%d hits_delta=%d",
						err, result.IsError, redirectApprovals, hits-before)
				}
			}
		})
	}
}

func TestRuleResourceNamespaces(t *testing.T) {
	workspace := t.TempDir()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "actual"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual", filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		resource string
		path     string
		invalid  bool
	}{
		{"denied.example", filepath.Join(canonical, "denied.example"), false},
		{"linked/file", filepath.Join(canonical, "actual/file"), false},
		{filepath.Join(canonical, "absolute"), filepath.Join(canonical, "absolute"), false},
		{"../escape", "", true},
		{"https://denied.example/data", "", false},
	} {
		t.Run(test.resource, func(t *testing.T) {
			rule := policy.Rule{Tool: "*", Resource: test.resource, Action: policy.ActionDeny}
			err := canonicalizeRuleResource(&rule, canonical)
			if test.invalid {
				if err == nil {
					t.Fatal("escaping relative rule accepted")
				}
				return
			}
			if err != nil || rule.Resource != test.resource || rule.ResourcePath != test.path {
				t.Fatalf("canonical rule=%+v err=%v", rule, err)
			}
			runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto)
			runtime.Repository = []policy.Rule{rule}
			resources := []tool.Resource{{Kind: "agent", ID: test.resource, Access: tool.AccessRead}}
			if test.path != "" {
				resources = append(resources, tool.Resource{Kind: "file", Path: test.path, Access: tool.AccessRead})
			}
			for _, resource := range resources {
				decision := runtime.Evaluate(policy.Invocation{
					CallID: "namespace", Tool: "mixed", Capability: tool.CapabilityRead,
					Resources: []tool.Resource{resource}, Validated: true,
				})
				if decision.Action != policy.ActionDeny || decision.Code != "repository_rule_denied" {
					t.Fatalf("resource=%+v decision=%+v", resource, decision)
				}
			}
		})
	}
}
