import {createRoot} from "react-dom/client";
import {App} from "../../../src/ui/App";
import {RuntimeClient} from "../../../src/runtime/client";
import type {RuntimeEvent, SessionSummary} from "../../../src/protocol";
import "../../../src/ui/styles.css";
import "../../../src/ui/theme/components.css";

const client = new RuntimeClient();
const withdrawal = new URLSearchParams(location.search).has("withdrawal");
const scrolling = new URLSearchParams(location.search).get("scrolling");
// #region debug-point A:withdraw-click
if (new URLSearchParams(location.search).has("debug-withdrawal")) document.addEventListener("click", (event) => { if ((event.target as Element)?.closest('button[aria-label="Withdraw turn"]')) void fetch("http://127.0.0.1:7777/event", {method: "POST", body: JSON.stringify({sessionId: "turn-withdraw-no-feedback", runId: new URLSearchParams(location.search).get("debug-withdrawal"), hypothesisId: "A", location: "streaming:click", msg: "[DEBUG] Withdrawal clicked", data: {embedded: window.top !== window}, ts: Date.now()})}).catch(() => {}); }, true);
// #endregion
const session: SessionSummary = {
  version: 1, revision: 1, session_id: "streaming-fixture", thread_id: "thread",
  title: "Streaming fixture", status: "running", pinned: false, archived: false,
  isolation: "shared", workspace_root: "/fixture", workspace_label: "fixture",
  latest_sequence: 0, pending_approvals: 0, pending_inputs: 0, checkpoint_count: 0,
  changed_files: 0, total_tokens: 0, cost_microunits: 0, cost_known: true,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z"
};
if (withdrawal) session.latest_turn_id = "live";
// Exercise the real event batching and projection without network or durable state.
Object.assign(client, {
  state: {...client.getSnapshot(), phase: "ready", sessions: [session],
    selectedSessionID: session.session_id, workspaceRoot: "/fixture", socketConnected: true},
  start: async () => {},
  loadDraft: () => "",
  saveDraft: () => {},
  scheduleSessionRefresh: () => {},
  refreshUsage: async () => {},
  refreshProgress: async () => {},
  refreshTrace: async () => {},
  refreshSessions: async () => {},
  refreshWorkspaces: async () => {}
});
const receiver = client as unknown as {
  applyEvent: (event: RuntimeEvent, sessionID: string) => void;
};
let sequence = 0;
const emit = (kind: string, data: Record<string, unknown>, turnID = "live") => {
  sequence += 1;
  receiver.applyEvent({
    version: 1, sequence, id: `event-${sequence}`, kind, operation_id: turnID,
    thread_id: "thread", turn_id: turnID, item_id: `item-${sequence}`,
    created_at: "2026-01-01T00:00:00Z", data
  }, session.session_id);
};
if (withdrawal) {
  Object.assign(client, {
    withdrawTurn: async (turnID: string) => {
      // #region debug-point B:withdraw-request
      if (new URLSearchParams(location.search).has("debug-withdrawal")) void fetch("http://127.0.0.1:7777/event", {method: "POST", body: JSON.stringify({sessionId: "turn-withdraw-no-feedback", runId: new URLSearchParams(location.search).get("debug-withdrawal"), hypothesisId: "B", location: "streaming:withdrawTurn", msg: "[DEBUG] Withdrawal request reached client", data: {}, ts: Date.now()})}).catch(() => {});
      // #endregion
      emit("turn.canceled", {reason: "user_interrupted"}, turnID);
      emit("turn.withdrawn", {}, turnID);
      // #region debug-point C:withdraw-result
      if (new URLSearchParams(location.search).has("debug-withdrawal")) requestAnimationFrame(() => requestAnimationFrame(() => { void fetch("http://127.0.0.1:7777/event", {method: "POST", body: JSON.stringify({sessionId: "turn-withdraw-no-feedback", runId: new URLSearchParams(location.search).get("debug-withdrawal"), hypothesisId: "C", location: "streaming:projection", msg: "[DEBUG] Withdrawal projection", data: {withdrawn: Boolean(document.querySelector('[data-turn-id="live"] .withdrawnTurnToggle')), promptVisible: Boolean(document.querySelector('[data-turn-id="live"] .userMessage'))}, ts: Date.now()})}).catch(() => {}); }));
      // #endregion
    }
  });
}
const paragraph = [
  "A **structured result** with `inline code` and [reference](https://example.test).",
  "",
  "```typescript",
  "const result = values.map((value) => ({value, ready: true}));",
  "```",
  "",
  "| Item | State |",
  "| --- | --- |",
  "| Render | Ready |",
  ""
].join("\n");
for (let index = 0; index < (scrolling ? 10 : 60); index += 1) {
  emit("turn.started", {prompt: `Historical question ${index}`}, `history-${index}`);
  emit("turn.completed", {text: paragraph.repeat(3)}, `history-${index}`);
}
emit("turn.started", {prompt: "Stream a detailed answer"});
if (scrolling) {
  if (scrolling === "tool") {
    emit("tool.start", {call_id: "read-live", tool: "file_read", arguments: {path: "main.ts"}});
    emit("tool.result", {call_id: "read-live", tool: "file_read",
      output: Array.from({length: 16}, (_, index) => `line ${index + 1}: inspected code`).join("\n")});
  } else {
    emit("output.draft", {sample_id: "sample-live",
      text: Array.from({length: 24}, (_, index) => `Reading paragraph ${index}.\n\n`).join("")});
  }
  // Tests control event timing through the real client's projection/notifier.
  document.addEventListener("fixture:events", (event) => {
    for (const item of (event as CustomEvent<Array<{
      kind: string; data: Record<string, unknown>;
    }>>).detail) emit(item.kind, item.data);
  });
}
if (new URLSearchParams(location.search).has("reasoning")) {
  emit("reasoning.completed", {
    sample_id: "sample-live",
    text: Array.from({length: 80}, (_, index) =>
      `Reasoning line ${index + 1}: inspect the workspace and verify the result.`
    ).join("\n")
  });
}
const interactions = new URLSearchParams(location.search).has("interactions");
if (interactions) {
  emit("commentary.completed", {
    message_id: "commentary-live", sample_id: "sample-live",
    text: "Inspecting the workspace.", call_ids: ["read-live"]
  });
  emit("tool.start", {call_id: "read-live", tool: "file_read", arguments: {path: "main.ts"}});
  emit("tool.result", {
    call_id: "read-live", tool: "file_read",
    output: Array.from({length: 60}, (_, index) => `line ${index + 1}: sample content`).join("\n")
  });
  emit("output.delta", {text: "```typescript\nconst value = 1;\n```"});
}
createRoot(document.getElementById("root")!).render(<App client={client} />);

if (interactions) {
  const append = document.createElement("button");
  append.textContent = "Append while pressed";
  append.style.cssText = "position:fixed;top:0;right:0;z-index:99999";
  document.body.append(append);
  append.onclick = () => {
    document.querySelector(".transcript")?.addEventListener("pointerdown", () => {
      emit("output.delta", {text: `\n\n${paragraph.repeat(4)}`});
    }, {once: true});
    append.textContent = "Append armed";
  };
}
const start = document.createElement("button");
start.textContent = "Run streaming fixture";
start.style.cssText = "position:fixed;top:0;right:0;z-index:99999";
if (interactions) start.style.top = "28px";
document.body.append(start);
const output = document.createElement("output");
output.id = "streaming-results";
output.hidden = true;
document.body.append(output);
start.onclick = () => {
  start.disabled = true;
  const gaps: number[] = [];
  const longTasks: number[] = [];
  const observer = new PerformanceObserver((list) => {
    longTasks.push(...list.getEntries().map((entry) => entry.duration));
  });
  observer.observe({entryTypes: ["longtask"]});
  let previous = performance.now();
  let frame = 0;
  const stream = () => {
    const now = performance.now();
    gaps.push(now - previous);
    previous = now;
    emit("output.delta", {text: paragraph});
    frame += 1;
    if (frame < 40) {
      requestAnimationFrame(stream);
    } else {
      emit("turn.completed", {text: paragraph.repeat(frame)});
      requestAnimationFrame(() => requestAnimationFrame(() => {
        observer.disconnect();
        const ordered = gaps.slice(1).sort((a, b) => a - b);
        output.textContent = JSON.stringify({
          frames: frame, p95: ordered[Math.floor(ordered.length * 0.95)],
          max: Math.max(...ordered), longTasks: longTasks.length,
          longTaskMS: longTasks.reduce((sum, value) => sum + value, 0),
          expectedText: paragraph.repeat(frame).length
        });
        start.textContent = "Streaming fixture complete";
      }));
    }
  };
  requestAnimationFrame(stream);
};
