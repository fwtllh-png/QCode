package web

import "sort"

// RouteContract is the public transport shape for one Web RPC endpoint.
type RouteContract struct {
	Path               string `json:"path"`
	Method             string `json:"method"`
	Request            string `json:"request"`
	Response           string `json:"response"`
	Mutation           bool   `json:"mutation"`
	IdempotencyKey     bool   `json:"idempotency_key"`
	RequiresRuntime    bool   `json:"requires_runtime"`
	RequiresCapability bool   `json:"requires_capability"`
}

// HostContract is the generated Web transport contract source.
type HostContract struct {
	ID              string          `json:"contract_id"`
	Title           string          `json:"title"`
	ProtocolVersion int             `json:"protocol_version"`
	LoopbackOnly    bool            `json:"loopback_only"`
	SameOriginOnly  bool            `json:"same_origin_only"`
	Capacity        Capacity        `json:"capacity_defaults"`
	Routes          []RouteContract `json:"routes"`
}

var unaryRouteContracts = []RouteContract{
	setupRPC("setup/probe", "setup_probe_request", "setup_probe_result", false, false),
	setupRPC("setup/apply", "setup_request", "setup_result", true, true),
	setupRPC("workspace/list", "empty", "workspace_catalog", false, false),
	setupRPC(
		"workspace/select-directory",
		"empty",
		"workspace_directory",
		false,
		false,
	),
	setupRPC("workspace/add", "workspace_add", "workspace_add_result", true, true),
	setupRPC("connection/list", "empty", "connection_list", false, false),
	setupRPC("connection/add", "setup_request", "connection_list", true, true),
	setupRPC("connection/remove", "connection_id", "connection_list", true, true),
	setupRPC("connection/default", "connection_id", "connection_list", true, true),
	setupRPC(
		"workspace/remove",
		"workspace_remove",
		"workspace_catalog",
		true,
		true,
	),
	rpc("system/describe", "empty", "system_description", false, false),
	rpc("system/readiness", "empty", "runtime_readiness", false, false),
	rpc("system/diagnostics", "empty", "system_diagnostics", false, false),
	rpc("session/create", "session_create", "session_binding", true, true),
	rpc("session/activate", "session_identity", "session_binding", true, true),
	rpc("session/list", "session_list", "session_list", false, false),
	rpc("session/status", "session_identity", "session_summary", false, false),
	rpc("session/update", "session_update", "session_lifecycle_update", true, true),
	rpc("session/delete", "session_delete", "session_delete_result", true, true),
	rpc("session/history", "session_history", "session_history_page", false, false),
	rpc("session/snapshot", "session_identity", "presentation_snapshot", false, false),
	rpc("session/export", "session_identity", "session_export", false, false),
	rpc("session/merge", "session_merge", "merge_result", true, true),
	rpc("operation/submit", "operation_submit", "operation_receipt", true, true),
	rpc("profile/get", "session_identity", "session_profile_snapshot", false, false),
	rpc("profile/update", "profile_update", "profile_update_result", true, true),
	rpc("agent-preset/list", "agent_preset_list", "agent_preset_list", false, false),
	rpc("agent-preset/save", "agent_preset_save", "agent_preset_mutation", true, true),
	rpc("agent-preset/delete", "agent_preset_delete", "agent_preset_mutation", true, true),
	rpc("agent-preset/apply", "agent_preset_apply", "agent_preset_apply_result", true, true),
	rpc("provider/list", "empty", "provider_catalog", false, false),
	rpc("connection/status", "empty", "workspace_connection", false, false),
	rpc("model/list", "empty", "model_catalog", false, false),
	rpc("model/test", "model_test", "model_test_result", false, false),
	rpc("model/probe", "model_probe", "setup_probe_result", false, false),
	rpc("model/add", "model_mutation", "model_catalog", true, true),
	rpc("model/update", "model_mutation", "model_catalog", true, true),
	rpc("model/remove", "model_remove", "model_catalog", true, true),
	rpc("tool/catalog", "session_identity", "tool_catalog", false, false),
	rpc("checkpoint/list", "checkpoint_query", "checkpoint_list", false, false),
	rpc("checkpoint/get", "checkpoint_identity", "checkpoint", false, false),
	rpc("checkpoint/restore", "checkpoint_identity", "checkpoint_restore", true, true),
	rpc("checkpoint/fork", "checkpoint_fork", "checkpoint_fork_result", true, true),
	rpc("turn/recover", "turn_recover", "operation_receipt", true, true),
	rpc("turn/withdraw", "turn_withdraw", "empty", true, true),
	rpc("turn/queue", "session_identity", "turn_queue", false, false),
	rpc("plan/get", "session_identity", "session_plan", false, false),
	rpc("agent/list", "agent_query", "agent_list", false, false),
	rpc("trace/query", "trace_query", "trace_snapshot", false, false),
	rpc("usage/query", "usage_query", "usage_summary", false, false),
	rpc("extension/list", "extension_query", "extension_list", false, false),
	rpc("extension/control", "extension_control", "extension_control_result", true, true),
	rpc("workspace/browse", "workspace_browse", "workspace_entries", false, false),
	rpc("workspace/search", "workspace_search", "workspace_entries", false, false),
	rpc("workspace/resource", "workspace_resource", "workspace_resource", false, false),
	rpc("workspace/image", "workspace_resource", "workspace_image", false, false),
	rpc("workspace/symbols", "workspace_symbols", "workspace_symbol_list", false, false),
	rpc("workspace/diagnostics", "session_identity", "workspace_diagnostics", false, false),
	rpc("workspace/diff", "workspace_diff", "workspace_diff", false, false),
	rpc("workspace/git-status", "empty", "workspace_git_overview", false, false),
	rpc("workspace/git-action", "workspace_git_action", "workspace_git_result", true, true),
	rpc("workspace/git-diff", "workspace_git_diff", "workspace_git_patch", false, false),
	rpc("workspace/git-switch", "workspace_git_switch", "workspace_git", true, true),
	rpc("credential/status", "empty", "credential_status", false, false),
	rpc("credential/set-keyring", "credential_set", "credential_status", true, true),
	rpc("credential/clear-keyring", "empty", "credential_status", true, true),
	rpc("credential/validate", "empty", "credential_status", false, false),
	rpc("mcp/health", "empty", "mcp_health_list", false, false),
}

var unaryRouteSet = func() map[string]RouteContract {
	result := make(map[string]RouteContract, len(unaryRouteContracts))
	for _, route := range unaryRouteContracts {
		result[route.Path] = route
	}
	return result
}()

func rpc(path, request, response string, mutation, idempotency bool) RouteContract {
	return RouteContract{
		Path: path, Method: "POST", Request: request, Response: response,
		Mutation: mutation, IdempotencyKey: idempotency,
		RequiresRuntime: true, RequiresCapability: true,
	}
}

func setupRPC(path, request, response string, mutation, idempotency bool) RouteContract {
	return RouteContract{
		Path: path, Method: "POST", Request: request, Response: response,
		Mutation: mutation, IdempotencyKey: idempotency,
		RequiresRuntime: false, RequiresCapability: true,
	}
}

func unaryRouteContract(path string) (RouteContract, bool) {
	contract, ok := unaryRouteSet[path]
	return contract, ok
}

// Contract returns a detached and deterministically ordered transport contract.
func Contract() HostContract {
	routes := []RouteContract{
		{
			Path: "/healthz", Method: "GET", Request: "empty",
			Response: "health", RequiresRuntime: false, RequiresCapability: false,
		},
		{
			Path: "/api/v1/bootstrap", Method: "GET", Request: "empty",
			Response: "bootstrap", RequiresRuntime: false, RequiresCapability: false,
		},
		{
			Path: "/api/v1/events", Method: "GET+WEBSOCKET", Request: "auth_frame",
			Response: "event_frame", RequiresRuntime: true, RequiresCapability: true,
		},
		{
			Path: "/api/v1/content/{handle}", Method: "GET", Request: "content_handle",
			Response: "content_download", RequiresRuntime: true, RequiresCapability: true,
		},
	}
	for _, route := range unaryRouteContracts {
		copy := route
		copy.Path = "/api/v1/" + copy.Path
		routes = append(routes, copy)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path == routes[j].Path {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Path < routes[j].Path
	})
	return HostContract{
		ID:              "https://qcode.dev/contracts/web-host-v1.json",
		Title:           "QCode Web Host transport contract manifest",
		ProtocolVersion: webProtocol,
		LoopbackOnly:    true,
		SameOriginOnly:  true,
		Capacity:        defaultCapacity(),
		Routes:          routes,
	}
}
