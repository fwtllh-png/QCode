import {expect, test} from "@playwright/test";
import {spawn, type ChildProcessByStdio} from "node:child_process";
import {mkdtemp, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import path from "node:path";
import type {Readable} from "node:stream";
import {fileURLToPath} from "node:url";
import {writeFixtureConfig} from "./fixture-config";

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
let server: ChildProcessByStdio<null, Readable, Readable>;
let dataDir: string;
let workspaceDir: string;
let baseURL: string;

test.beforeEach(async () => {
  dataDir = await mkdtemp(path.join(tmpdir(), "qcode-empty-startup-state-"));
  workspaceDir = await mkdtemp(path.join(tmpdir(), "qcode-explicit-workspace-"));
  server = spawn(process.env.QCODE_E2E_BINARY || path.join(repositoryRoot, "bin/qcode"), [
    "--config", await writeFixtureConfig(path.join(dataDir, "runtime.toml")),
    "--data-dir", dataDir,
    "--provider-fixture", path.join(repositoryRoot, "testdata/providers/web-visual"),
    "--port", "0"
  ], {cwd: repositoryRoot, stdio: ["ignore", "pipe", "pipe"]});
  baseURL = await new Promise<string>((resolve, reject) => {
    let output = "";
    let diagnostics = "";
    server.stderr.on("data", (chunk: Buffer) => { diagnostics += chunk.toString(); });
    server.stdout.on("data", (chunk: Buffer) => {
      output += chunk.toString();
      const match = output.match(/QCode Runtime Ready: (http:\/\/127\.0\.0\.1:\d+\/\S*)/);
      if (match) resolve(match[1]);
    });
    server.once("error", reject);
    server.once("exit", (code) => reject(new Error(`Supervisor exited ${code}: ${diagnostics}`)));
  });
});

test.afterEach(async () => {
  if (server && server.exitCode === null) {
    server.kill("SIGINT");
    await Promise.race([
      new Promise<void>((resolve) => server.once("exit", () => resolve())),
      new Promise<void>((resolve) => setTimeout(resolve, 10_000))
    ]);
    if (server.exitCode === null) server.kill("SIGKILL");
  }
  if (dataDir) await rm(dataDir, {recursive: true, force: true});
  if (workspaceDir) await rm(workspaceDir, {recursive: true, force: true});
});

test("plain startup from the QCode repository has no default Workspace", async ({page}) => {
  let sockets = 0;
  page.on("websocket", () => { sockets++; });
  await page.setViewportSize({width: 1440, height: 900});
  await page.goto(baseURL);
  await expect(page.getByRole("heading", {name: "Choose a workspace"})).toBeVisible();
  await expect(page.locator(".workspaceGroup")).toHaveCount(0);
  await expect(page.getByRole("button", {name: "Add workspace", exact: true})).toBeVisible();
  await expect(page.getByRole("button", {name: "Git tools", exact: true})).toHaveCount(0);
  await expect(page.getByPlaceholder("Ask QCode")).toHaveCount(0);
  expect(sockets).toBe(0);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/workspace-empty-desktop.png")});
  await page.setViewportSize({width: 390, height: 844});
  await expect(page.getByRole("heading", {name: "Choose a workspace"})).toBeVisible();
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth
  )).toBe(true);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/workspace-empty-mobile.png")});
});

test("only the explicitly selected folder is added and removing it preserves the empty state", async ({page}) => {
  await page.route("**/workspace/select-directory", (route) => route.fulfill({
    contentType: "application/json",
    body: JSON.stringify({version: 1, result: {path: workspaceDir, cancelled: false}})
  }));
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Add workspace", exact: true}).click();
  await page.getByRole("dialog", {name: "Workspaces", exact: true})
    .getByRole("button", {name: "Choose folder", exact: true}).click();
  await expect(page.locator(".workspaceGroup")).toHaveCount(1);
  await expect(page.locator(".workspaceGroup")).toContainText(path.basename(workspaceDir));
  await expect(page.getByRole("heading", {name: "Start a new session"})).toBeVisible();
  await page.goto(baseURL);
  await expect(page.getByRole("heading", {name: "Choose a workspace"})).toBeVisible();
  await expect(page.locator(".workspaceGroup")).toHaveCount(1);
  await page.locator(".workspaceGroup").hover();
  await page.getByRole("button", {name: `Remove ${path.basename(workspaceDir)}`, exact: true}).click();
  await page.getByRole("alertdialog").getByRole("button", {name: "Remove workspace", exact: true}).click();
  await expect(page.locator(".workspaceGroup")).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole("heading", {name: "Choose a workspace"})).toBeVisible();
  await expect(page.locator(".workspaceGroup")).toHaveCount(0);
});
