import {createRoot} from "react-dom/client";
import {App} from "../../../src/ui/App";
import {RuntimeClient} from "../../../src/runtime/client";
import type {RuntimeEvent, SessionSummary} from "../../../src/protocol";
import "../../../src/ui/styles.css";

const client = new RuntimeClient();
const mode = new URLSearchParams(location.search).get("mode") ?? "allow";
const session: SessionSummary = {
  version: 1, revision: 1, session_id: "guardian-fixture", thread_id: "thread",
  title: "Guardian fixture", status: "running", pinned: false, archived: false,
  isolation: "shared", workspace_root: "/fixture", workspace_label: "fixture",
  latest_sequence: 0, pending_approvals: 0, pending_inputs: 0, checkpoint_count: 0,
  changed_files: 0, total_tokens: 0, cost_microunits: 0, cost_known: true,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z"
};
Object.assign(client, {
  state: {...client.getSnapshot(), phase: "ready", sessions: [session],
    selectedSessionID: session.session_id, workspaceRoot: "/fixture", socketConnected: true},
  start: async () => {}, loadDraft: () => "", saveDraft: () => {},
  scheduleSessionRefresh: () => {}, refreshUsage: async () => {},
  refreshProgress: async () => {}, refreshTrace: async () => {},
  refreshSessions: async () => {}, refreshWorkspaces: async () => {},
  decideApproval: async (requestID: string, decision: string) => {
    emit("approval.resolved", {request_id: requestID, decision});
    emit("tool.result", {call_id: "call", tool: "exec_command", output: decision === "approve" ? "done" : "Approval denied", is_error: decision !== "approve",
      execution: {guardian_review_id: "review", terminal_status: decision === "approve" ? "succeeded" : "rejected"}});
    emit("turn.completed", {text: "Finished"});
  },
  cancel: async () => { emit("approval.resolved", {request_id: "approval", decision: "cancel"}); emit("turn.canceled", {reason: "user_interrupted"}); }
});
const receiver = client as unknown as {applyEvent: (event: RuntimeEvent, sessionID: string) => void};
const key = `guardian-fixture-${mode}`;
const events: RuntimeEvent[] = JSON.parse(sessionStorage.getItem(key) ?? "[]");
function emit(kind: string, data: Record<string, unknown>) {
  const sequence = events.length + 1;
  const event: RuntimeEvent = {version: 1, sequence, id: `event-${sequence}`, kind,
    operation_id: "turn", thread_id: "thread", turn_id: "turn", item_id: `item-${sequence}`,
    created_at: "2026-01-01T00:00:00Z", data};
  events.push(event);
  sessionStorage.setItem(key, JSON.stringify(events));
  receiver.applyEvent(event, session.session_id);
}
for (const event of events) receiver.applyEvent(event, session.session_id);
if (!events.length) {
  emit("turn.started", {prompt: "Run the build"});
  emit("tool.start", {call_id: "call", tool: "exec_command", arguments: {command: "./build.sh", write_paths: ["generated"]}});
  emit("guardian.review", {call_id: "call", review_id: "review", provider: "judge", model: "model",
    phase: mode === "allow" ? "decided" : "failed", reason_code: mode === "allow" ? "policy_decision" : "review_timed_out",
    ...(mode === "allow" ? {assessment: {risk: "low", authorization: "supported", recommendation: "allow"},
      decision: {action: "allow", authority: "guardian", code: "guardian_allowed"}} : {})});
  if (mode !== "allow") emit("approval.required", {request_id: "approval", call_id: "call", tool: "exec_command",
    arguments: {command: "./build.sh", write_paths: ["generated"]}, allowed_scopes: ["once"], replacement_allowed: false,
    effect: "process.mutating", resources: [{kind: "directory", path: "/fixture/generated", access: "write"}],
    guardian_review_id: "review", guardian_reason_code: "review_timed_out"});
}
createRoot(document.getElementById("root")!).render(<App client={client} />);
