import {expect, test, type Page} from "@playwright/test";
import {spawn, type ChildProcessByStdio} from "node:child_process";
import {createServer, type Server} from "node:http";
import {mkdtemp, mkdir, readFile, readdir, rm, stat, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import path from "node:path";
import type {Readable} from "node:stream";
import {fileURLToPath} from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const binary = process.env.QCODE_E2E_BINARY || path.join(root, "bin/qcode");
type RuntimeProcess = ChildProcessByStdio<null, Readable, Readable>;

// Only the external model endpoint is simulated. Browser APIs, WebSocket
// replay, Host, Wire, the disk store, recovery and process execution are real.
for (const decision of ["approve", "deny", "cancel", "changed_resource"] as const) {
  test(`Guardian pending approval survives a real Runtime crash and ${decision}`, async ({page}, testInfo) => {
    test.setTimeout(120_000);
    const state = await mkdtemp(path.join(tmpdir(), "qcode-guardian-state-"));
    const workspace = await mkdtemp(path.join(tmpdir(), "qcode-guardian-workspace-"));
    let child: RuntimeProcess | undefined;
    let provider: Server | undefined;
    let actCalls = 0;
    let judgeCalls = 0;
    const errors: string[] = [];
    const diagnostics: string[] = [];
    const frames: string[] = [];
    page.on("websocket", (socket) => socket.on("framereceived", (frame) => frames.push(String(frame.payload))));
    const command = "printf executed >> generated/once.txt";
    const incompleteSummary = decision === "changed_resource" ? "The restored operation changed" : "The write was declined";
    try {
      await mkdir(path.join(workspace, "generated"));
      provider = createServer(async (request, response) => {
        try {
          const chunks: Buffer[] = [];
          for await (const chunk of request) chunks.push(Buffer.from(chunk));
          const body = JSON.parse(Buffer.concat(chunks).toString());
          let delta: object;
          let finish = "stop";
          const reviewInput = body.messages?.find((message: {role: string; content: unknown}) =>
            message.role === "user" && typeof message.content === "string" &&
            message.content.includes('"untrusted_operation"'));
          if (reviewInput) {
            judgeCalls++;
            const sources = JSON.parse(reviewInput.content).user_sources;
            delta = {content: JSON.stringify({risk_level: "low", authorization: "supported",
              authorization_source_ids: [sources[0].Source.ID], recommendation: "prompt",
              rationale: "The fixture requires the user to decide this bounded write."})};
          } else if (!body.tools?.length) {
            delta = {content: "Guardian recovery"};
          } else {
            actCalls++;
            const steps = [
              ["submit_plan", {steps: [{id: "write", title: "Write the requested file", status: "pending"}]}],
              ["exec_command", {command, write_paths: ["generated/once.txt"], yield_time_ms: 30_000}],
              ...(decision === "approve" ? [["update_plan", {steps: [{id: "write", title: "Write the requested file", status: "done"}]}]] : []),
              ["turn_complete", decision === "approve"
                ? {status: "complete", summary: "Guardian recovery complete", pending_actions: []}
                : {status: "incomplete", summary: incompleteSummary, pending_actions: [incompleteSummary]}]
            ] as const;
            const step = steps[actCalls - 1];
            if (!step) throw new Error(`unexpected act call ${actCalls}`);
            delta = {tool_calls: [{index: 0, id: `guardian-call-${actCalls}`, type: "function",
              function: {name: step[0], arguments: JSON.stringify(step[1])}}]};
            finish = "tool_calls";
          }
          response.writeHead(200, {"Content-Type": "text/event-stream"});
          response.end(`data: ${JSON.stringify({choices: [{index: 0, delta, finish_reason: finish}],
            usage: {prompt_tokens: 100, completion_tokens: 30, total_tokens: 130}})}\n\ndata: [DONE]\n\n`);
        } catch (error) {
          errors.push(String(error));
          response.writeHead(500).end("fixture request failed");
        }
      });
      await new Promise<void>((resolve) => provider!.listen(0, "127.0.0.1", resolve));
      const address = provider.address();
      if (!address || typeof address === "string") throw new Error("provider has no address");
      const metadata = path.join(state, "model.json");
      await writeFile(metadata, JSON.stringify({canonical_id: "guardian-fixture", wire_id: "guardian-fixture",
        context_tokens: 1_000_000, max_output_tokens: 16_384,
        capabilities: {streaming: true, tool_calls: true},
        pricing: {input_per_million: 0, output_per_million: 0, currency: "USD"}}));
      const config = path.join(state, "runtime.toml");
      await writeFile(config, `[execution]\nprovider = "guardian-fixture"\nmodel = "guardian-fixture"\n` +
        `workspace = ${JSON.stringify(workspace)}\ntools = true\nprotocol = "openai_chat"\n` +
        `base_url = "http://127.0.0.1:${address.port}"\nmodel_metadata = ${JSON.stringify(metadata)}\n` +
        `[security.guardian]\nenabled = true\ntimeout = "5s"\nmax_output_tokens = 1024\n`, {mode: 0o600});
      const launch = async (port: number) => {
        child = spawn(binary, ["--config", config, "--data-dir", state, "--port", String(port)], {
          cwd: root, stdio: ["ignore", "pipe", "pipe"],
          env: {...process.env, QCODE_DISABLE_APPROVAL_AUTO_REVIEW: ""}
        });
        child.stderr.on("data", (chunk: Buffer) => diagnostics.push(chunk.toString()));
        return runtimeURL(child);
      };
      const url = await launch(0);
      await page.goto(url);
      await page.getByRole("button", {name: "Create session", exact: true}).click();
      await page.getByPlaceholder("Ask QCode").fill("Create generated/once.txt containing executed. Ask me to approve the command.");
      await page.getByRole("button", {name: "Send", exact: true}).click();
      const approval = page.locator(".approvalComposer");
      await expect(approval).toBeVisible();
      await expect(approval).toContainText(command);
      expect(judgeCalls).toBe(1);
      expect(actCalls).toBe(2);
      const requestID = await approvalRequestID(page);
      const reviewID = runtimeEvents(frames).find((event) => event.kind === "guardian.review")?.data.review_id;
      expect(reviewID).toBeTruthy();
      expect(await exists(path.join(workspace, "generated/once.txt"))).toBe(false);

      // Graceful shutdown intentionally cancels turns. SIGKILL reproduces an
      // interrupted process with its committed pending approval still on disk.
      await stop(child!, "SIGKILL");
      if (decision === "changed_resource") await mkdir(path.join(workspace, "generated/once.txt"));
      const restarted = await launch(Number(new URL(url).port));
      await page.goto(restarted);
      await expect(approval).toBeVisible();
      await expect(approval).toContainText(command);
      expect(await approvalRequestID(page)).toBe(requestID);
      expect(judgeCalls).toBe(1);
      expect(actCalls).toBe(2);
      if (decision !== "changed_resource") expect(await exists(path.join(workspace, "generated/once.txt"))).toBe(false);

      await approval.getByRole("button", {name: decision === "deny" ? "Deny" : decision === "cancel" ? "Stop turn" : "Approve once", exact: true}).click();
      await expect.poll(() => runtimeEvents(frames).some((event) =>
        ["turn.completed", "turn.failed", "turn.canceled"].includes(event.kind)), {timeout: 45_000}).toBe(true);
      await expect(approval).toHaveCount(0);
      const events = runtimeEvents(frames);
      const failures = events.filter((event) => event.kind === "turn.failed");
      if (decision === "deny" || decision === "changed_resource") {
        expect(failures).toHaveLength(1);
        expect(failures[0].data.convergence?.cause).toBe("declared_incomplete");
      } else expect(failures).toEqual([]);
      expect(events.flatMap((event) => event.data.secondary_issues || [])).toEqual([]);
      expect(events.filter((event) => event.kind === "approval.required")).toHaveLength(1);
      const resolved = events.filter((event) => event.kind === "approval.resolved");
      expect(resolved).toHaveLength(decision === "cancel" ? 0 : 1);
      if (decision !== "cancel") expect(resolved[0].data.request_id).toBe(requestID);
      if (decision === "approve") {
        await expect(page.getByText("Guardian recovery complete", {exact: true}).last()).toBeVisible();
        expect(await readFile(path.join(workspace, "generated/once.txt"), "utf8")).toBe("executed");
        const results = events.filter((event) => event.kind === "tool.result" && event.data.call_id === "guardian-call-2");
        expect(results).toHaveLength(1);
        expect(results[0].data.is_error).toBeFalsy();
        expect(results[0].data.execution?.guardian_review_id).toBe(reviewID);
        expect(actCalls).toBe(4);
      } else {
        await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
        if (decision === "changed_resource") {
          expect((await stat(path.join(workspace, "generated/once.txt"))).isDirectory()).toBe(true);
          expect(await readdir(path.join(workspace, "generated/once.txt"))).toEqual([]);
          const result = events.find((event) => event.kind === "tool.result" && event.data.call_id === "guardian-call-2");
          expect(result?.data.is_error).toBe(true);
          expect(result?.data.output).toContain("restored approval no longer matches");
          expect(result?.data.execution?.attempts || []).toHaveLength(0);
        } else expect(await exists(path.join(workspace, "generated/once.txt"))).toBe(false);
        if (decision !== "cancel") await expect(page.getByText(incompleteSummary, {exact: true}).last()).toBeVisible();
        expect(actCalls).toBe(decision === "cancel" ? 2 : 3);
      }
      await page.reload();
      await expect(page.getByText("Connected", {exact: true})).toBeVisible();
      await expect(approval).toHaveCount(0);
      expect(judgeCalls).toBe(1);
      expect(errors).toEqual([]);
    } catch (error) {
      if (child) await stop(child, "SIGQUIT");
      await writeFile(testInfo.outputPath("runtime-diagnostics.txt"), diagnostics.join(""));
      await writeFile(testInfo.outputPath("runtime-events.jsonl"), frames.join("\n"));
      await testInfo.attach("runtime-diagnostics", {path: testInfo.outputPath("runtime-diagnostics.txt"), contentType: "text/plain"});
      await testInfo.attach("runtime-events", {path: testInfo.outputPath("runtime-events.jsonl"), contentType: "application/x-ndjson"});
      await testInfo.attach("provider-counts", {body: JSON.stringify({actCalls, judgeCalls, errors}), contentType: "application/json"});
      throw error;
    } finally {
      if (child) await stop(child, "SIGKILL");
      if (provider) await new Promise<void>((resolve) => provider!.close(() => resolve()));
      await rm(state, {recursive: true, force: true});
      await rm(workspace, {recursive: true, force: true});
    }
  });
}

type RuntimeEvent = {id: string; kind: string; data: {request_id?: string; review_id?: string;
  call_id?: string; is_error?: boolean; output?: string; execution?: {guardian_review_id?: string; attempts?: unknown[]};
  convergence?: {cause: string}; secondary_issues?: unknown[]}};

function runtimeEvents(frames: string[]): RuntimeEvent[] {
  const events = new Map<string, RuntimeEvent>();
  for (const frame of frames) {
    const envelope = JSON.parse(frame);
    if (envelope.type === "event") events.set(envelope.event.id, envelope.event);
  }
  return [...events.values()];
}

async function exists(file: string): Promise<boolean> {
  try { await readFile(file); return true; }
  catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return false; throw error; }
}

async function approvalRequestID(page: Page): Promise<string> {
  // This reads the real DOM identity; no API response or client action is mocked.
  return page.locator(".approvalComposer").getAttribute("data-approval-key").then((value) => {
    if (!value) throw new Error("approval card has no request identity");
    return value;
  });
}

async function stop(child: RuntimeProcess, signal: NodeJS.Signals): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const done = new Promise<void>((resolve) => child.once("exit", () => resolve()));
  child.kill(signal);
  await done;
}

async function runtimeURL(child: RuntimeProcess): Promise<string> {
  return new Promise((resolve, reject) => {
    let output = "";
    let diagnostics = "";
    const timer = setTimeout(() => reject(new Error(`Runtime startup timed out: ${diagnostics}`)), 45_000);
    child.stderr.on("data", (chunk: Buffer) => { diagnostics += chunk.toString(); });
    child.stdout.on("data", (chunk: Buffer) => {
      output += chunk.toString();
      const match = output.match(/QCode Runtime Ready: (http:\/\/127\.0\.0\.1:\d+\/\S*)/);
      if (match) { clearTimeout(timer); resolve(match[1]); }
    });
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
    child.once("exit", (code) => { clearTimeout(timer); reject(new Error(`Runtime exited ${code}: ${diagnostics}`)); });
  });
}
