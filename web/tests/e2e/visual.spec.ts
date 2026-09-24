import {expect, test, type Locator, type Page} from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import {
  execFileSync,
  spawn,
  type ChildProcessByStdio
} from "node:child_process";
import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import path from "node:path";
import type {Readable} from "node:stream";
import {fileURLToPath} from "node:url";
import type {WorkspaceCatalog} from "../../src/protocol";

const repositoryRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../.."
);

let server: ChildProcessByStdio<null, Readable, Readable>;
let dataDir: string;
let workspaceDir: string;
let baseURL: string;

test.describe.configure({mode: "default"});
test.setTimeout(90_000);

test.beforeEach(async () => {
  dataDir = await mkdtemp(path.join(tmpdir(), "qcode-web-visual-"));
  workspaceDir = await mkdtemp(path.join(tmpdir(), "qcode-web-visual-workspace-"));
  await writeFile(
    path.join(workspaceDir, "README.md"),
    "# Visual fixture\n\nA stable baseline for browser goldens.\n"
  );
  await writeFile(
    path.join(workspaceDir, "main.go"),
    "package main\n\nfunc stableVisualFixture() {}\n"
  );
  execFileSync("git", ["init", "-q"], {cwd: workspaceDir});
  execFileSync("git", ["add", "."], {cwd: workspaceDir});
  execFileSync(
    "git",
    [
      "-c", "core.hooksPath=/dev/null",
      "-c", "user.name=QCode",
      "-c", "user.email=fixture@qcode.invalid",
      "commit", "-qm", "visual baseline"
    ],
    {cwd: workspaceDir}
  );
  server = spawn(
    process.env.QCODE_E2E_BINARY || path.join(repositoryRoot, "bin/qcode"),
    [
      "--workspace", workspaceDir,
      "--data-dir", dataDir,
      "--provider-fixture",
      path.join(repositoryRoot, "testdata/providers/web-visual"),
      "--provider", "openai",
      "--model", "fixture-model",
      "--enable-tools",
      "--posture", "suggest",
      "--port", "0",
      "--no-open"
    ],
    {
      cwd: repositoryRoot,
      stdio: ["ignore", "pipe", "pipe"]
    }
  );
  baseURL = await runtimeURL(server);
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

test.beforeEach(async ({page}) => {
  await page.setViewportSize({width: 1440, height: 900});
  await page.emulateMedia({
    colorScheme: "light",
    forcedColors: "none",
    reducedMotion: "reduce"
  });
  await page.goto(baseURL);
  await expect(page.locator(".app")).toBeVisible();
});

for (const width of [1440, 390]) {
  test(`automatic history scrolling preserves the anchor and bounds DOM at ${width}px`, async ({page}) => {
    await createSession(page);
    await installHistorySnapshot(page, 1, 500);
    await page.reload();
    await page.setViewportSize({width, height: 900});
    await page.getByRole("button", {name: "Close Git tools", exact: true}).click();
    const scrollport = page.locator("[data-conversation-scroll]");
    await expect(page.getByText("History message 500", {exact: true})).toBeVisible();
    await expect(page.locator("[data-entry-id]")).toHaveCount(200);
    const anchor = await historyScrollAnchor(scrollport, "top");
    await expect(page.getByText("History message 300", {exact: true})).toHaveCount(1);
    await expect(page.locator("[data-entry-id]")).toHaveCount(200);
    await expect.poll(async () => Math.abs(
      await transcriptAnchorTop(page.locator(`[data-entry-id="${anchor.id}"]`)) - anchor.top
    )).toBeLessThan(2);
    await expect(page.getByRole("button", {name: /^(Earlier|Newer) messages$/})).toHaveCount(0);
    await page.screenshot({path: path.join(repositoryRoot, `.tmp/history-auto-${width}.png`)});
    await historyScrollAnchor(scrollport, "bottom");
    await expect(page.getByText("History message 500", {exact: true})).toHaveCount(1);
    await expect(page.locator("[data-entry-id]")).toHaveCount(200);
    await historyScrollAnchor(scrollport, "top");
    await expect(page.getByText("History message 300", {exact: true})).toHaveCount(1);
    await page.getByRole("button", {name: "Back to bottom", exact: true}).click();
    await expect(page.getByText("History message 500", {exact: true})).toBeVisible();
    await expect.poll(() => scrollport.evaluate((node) =>
      node.scrollHeight - node.scrollTop - node.clientHeight
    )).toBeLessThanOrEqual(24);
    if (width === 1440) {
      await page.keyboard.press("Control+f");
      const search = page.getByRole("dialog", {name: "Search conversation", exact: true});
      await search.getByRole("tab", {name: "Turns", exact: true}).click();
      await search.getByRole("combobox", {name: "Search conversation", exact: true}).fill("History message 1");
      await search.getByRole("option").first().click();
      await expect(page.getByText("History message 1", {exact: true})).toBeVisible();
      const entry = page.locator('[data-navigation-current]').first();
      const top = await transcriptAnchorTop(entry);
      const id = await entry.getAttribute("data-entry-id");
      await page.getByRole("button", {name: "Trajectory", exact: true}).click();
      await expect(page.getByLabel("Execution trajectory")).toBeVisible();
      await page.getByRole("button", {name: "Chat", exact: true}).click();
      await expect.poll(async () => Math.abs(
        await transcriptAnchorTop(page.locator(`[data-entry-id="${id}"]`)) - top
      )).toBeLessThan(2);
    }
  });
}

test("automatic history loading retries explicitly and preserves reading during a delayed response", async ({page}) => {
  await createSession(page);
  await installHistorySnapshot(page, 901, 40, true);
  let requests = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/v1/session/history", async (route) => {
    requests++;
    expect(route.request().postDataJSON().before_sequence).toBe(901);
    if (requests === 1) {
      await route.fulfill({status: 503, contentType: "application/json", body: JSON.stringify({
        version: 1, problem: {version: 1, code: "unavailable", message: "History is temporarily offline", retryable: true}
      })});
      return;
    }
    await gate;
    await route.fulfill({contentType: "application/json", body: JSON.stringify({
      version: 1, result: {events: historyFixtureEvents(701, 200), more_before: false}
    })});
  });
  await page.reload();
  await page.getByRole("button", {name: "Close Git tools", exact: true}).click();
  await expect(page.getByText("History message 940", {exact: true})).toBeVisible();
  const scrollport = page.locator("[data-conversation-scroll]");
  await historyScrollAnchor(scrollport, "top");
  await expect(page.getByRole("button", {name: "Retry loading history", exact: true})).toBeVisible();
  await scrollport.evaluate((node) => node.dispatchEvent(new Event("scroll")));
  expect(requests).toBe(1);
  await page.getByRole("button", {name: "Retry loading history", exact: true}).click();
  await expect(page.getByRole("status", {name: "Loading earlier messages"})).toBeVisible();
  const anchor = await historyScrollAnchor(scrollport, 260);
  await expect.poll(() => requests).toBe(2);
  release();
  await expect(page.getByRole("status", {name: "Loading earlier messages"})).toHaveCount(0);
  await expect(page.locator("[data-entry-id]")).toHaveCount(200);
  await expect.poll(async () => Math.abs(
    await transcriptAnchorTop(page.locator(`[data-entry-id="${anchor.id}"]`)) - anchor.top
  )).toBeLessThan(2);
  expect(requests).toBe(2);
  await expect(page.getByRole("button", {name: "Retry loading history", exact: true})).toHaveCount(0);
});

async function installHistorySnapshot(page: Page, first: number, count: number, moreBefore = false): Promise<void> {
  await page.route("**/api/v1/session/snapshot", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.result.events = historyFixtureEvents(first, count);
    body.result.through_sequence = first + count - 1;
    body.result.history_truncated_before = moreBefore ? first - 1 : 0;
    await route.fulfill({response, json: body});
  });
}

function historyFixtureEvents(first: number, count: number) {
  return Array.from({length: count}, (_, index) => {
    const sequence = first + index;
    return {
      version: 1, id: `history-event-${sequence}`, kind: "turn.completed",
      operation_id: `history-operation-${sequence}`, thread_id: "history-thread",
      turn_id: `history-turn-${sequence}`, item_id: `history-item-${sequence}`,
      sequence, created_at: "2026-01-01T00:00:00Z",
      data: {text: `History message ${sequence}\n\n${"A recorded answer with variable height.\n\n".repeat(sequence % 3 + 1)}`, outcome: "answered"}
    };
  });
}

async function historyScrollAnchor(scrollport: Locator, target: "top" | "bottom" | number) {
  return scrollport.evaluate((node, target) => {
    node.scrollTop = target === "top" ? 0 : target === "bottom" ? node.scrollHeight : target;
    node.dispatchEvent(new Event("scroll"));
    const viewport = node.getBoundingClientRect();
    const anchor = Array.from(node.querySelectorAll<HTMLElement>("[data-entry-id]")).find((item) => {
      const box = item.firstElementChild!.getBoundingClientRect();
      return box.bottom > viewport.top && box.top < viewport.bottom;
    })!;
    return {id: anchor.dataset.entryId!, top: anchor.firstElementChild!.getBoundingClientRect().top - viewport.top};
  }, target);
}

test("Git tools inspect real staged and unstaged changes and switch local branches", async ({page}) => {
  await expect(page.getByRole("complementary", {name: "Git tools"})).toBeVisible();
  await expect(page.locator(".workspaceGroup select")).toHaveCount(0);
  await createSession(page);
  await writeFile(path.join(workspaceDir, "README.md"), "# Visual fixture\n\nStaged change.\n");
  execFileSync("git", ["add", "README.md"], {cwd: workspaceDir});
  await writeFile(path.join(workspaceDir, "main.go"), "package main\n\nfunc changedByGitTools() {}\n");
  await writeFile(path.join(workspaceDir, "new.txt"), "New file\n");
  execFileSync("git", ["branch", "git-tools-feature"], {cwd: workspaceDir});
  const panel = page.getByRole("complementary", {name: "Git tools"});
  await panel.getByRole("button", {name: "Refresh Git"}).click();
  await expect(panel.getByRole("button", {name: /Changes/})).toContainText("3 files");
  await expect(panel.getByText("Progress", {exact: true})).toHaveCount(0);
  await expect(panel.getByText("Agents", {exact: true})).toHaveCount(0);
  await panel.screenshot({path: path.join(repositoryRoot, ".tmp/git-tools-overview.png")});
  await panel.getByRole("button", {name: /Changes/}).click();
  await panel.getByRole("button", {name: /main.go/}).click();
  await expect(panel.getByLabel("Git file diff")).toContainText("changedByGitTools");
  await panel.getByLabel("Git change scope").selectOption("staged");
  await panel.getByRole("button", {name: /README.md/}).click();
  await expect(panel.getByLabel("Git file diff")).toContainText("Staged change");
  await assertViewportGeometry(page);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/git-tools-review.png")});
  await panel.getByRole("button", {name: "Back to Git tools"}).click();
  const branch = execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim();
  await panel.getByRole("button", {name: branch, exact: true}).click();
  await panel.getByRole("searchbox", {name: "Search Git branches"}).fill("git-tools-feature");
  await panel.getByRole("button", {name: "git-tools-feature", exact: true}).click();
  await expect(panel.getByRole("searchbox", {name: "Search Git branches"})).toHaveCount(0);
  await expect(panel.locator(".gitSummary").getByRole("button", {name: "git-tools-feature", exact: true})).toBeVisible();
  expect(execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim())
    .toBe("git-tools-feature");
  await panel.getByRole("button", {name: "Close Git tools"}).click();
  await expect(page.getByRole("button", {name: "Git tools", exact: true})).toBeFocused();
});

for (const viewport of [
  {width: 1440, height: 900},
  {width: 1024, height: 768},
  {width: 390, height: 844},
  {width: 844, height: 390}
]) {
  test(`Git diff fills its viewer and preserves virtual scrolling at ${viewport.width}x${viewport.height}`, async ({page}) => {
    await page.setViewportSize(viewport);
    await page.emulateMedia({colorScheme: viewport.width === 1024 || viewport.height === 390 ? "dark" : "light"});
    const filename = "long-diff-with-a-descriptive-filename.md";
    await writeFile(path.join(workspaceDir, filename),
      Array.from({length: 1000}, (_, index) => `Line ${index + 1}: ${"readable source content ".repeat(12)}`).join("\n") + "\n");
    let queries = 0;
    page.on("request", (request) => {
      if (request.url().endsWith("/workspace/git-diff")) queries++;
    });
    const panel = page.locator("#git-tools");
    await panel.getByRole("button", {name: "Refresh Git"}).click();
    await panel.getByRole("button", {name: /Changes/}).click();
    await panel.getByRole("button", {name: filename, exact: false}).click();
    const lines = panel.getByRole("region", {name: "Diff lines"});
    await expect(lines).toContainText("Line 1:");
    const initial = await lines.boundingBox();
    const bounds = await panel.boundingBox();
    expect(initial!.height).toBeGreaterThan(100);
    expect(bounds!.y + bounds!.height - initial!.y - initial!.height).toBeLessThanOrEqual(12);
    if (viewport.width > 720) {
      expect(initial!.width).toBeGreaterThan(bounds!.width * 0.65);
      expect(initial!.height).toBeGreaterThan(bounds!.height - 210);
    }
    await page.screenshot({path: path.join(repositoryRoot, `.tmp/git-diff-default-${viewport.width}x${viewport.height}.png`)});
    const queryCount = queries;
    await lines.evaluate((node) => { node.scrollTop = 5000; });
    await expect.poll(() => lines.evaluate((node) => node.scrollTop)).toBe(5000);
    await panel.getByRole("button", {name: "Hide changed files"}).click();
    await expect(panel.locator(".gitFileList")).toBeHidden();
    await expect.poll(async () => {
      const current = await lines.boundingBox();
      return viewport.width > 720 ? current!.width - initial!.width : current!.height - initial!.height;
    }).toBeGreaterThan(50);
    await expect.poll(() => lines.evaluate((node) => node.scrollTop)).toBe(5000);
    await panel.getByRole("button", {name: "Expand diff viewer"}).click();
    await expect(panel).toHaveAttribute("aria-modal", "true");
    await expect.poll(async () => (await panel.boundingBox())!.width).toBe(viewport.width - 24);
    await expect.poll(async () => (await panel.boundingBox())!.height).toBe(viewport.height - 24);
    await expect.poll(() => lines.evaluate((node) => node.scrollTop)).toBe(5000);
    await assertViewportGeometry(page);
    await page.screenshot({path: path.join(repositoryRoot, `.tmp/git-diff-expanded-${viewport.width}x${viewport.height}.png`)});
    await lines.evaluate((node) => { node.scrollTop = node.scrollHeight; });
    await expect(lines).toContainText("Line 1000:");
    const geometry = await lines.evaluate((node) => ({
      height: node.clientHeight, row: parseFloat(getComputedStyle(node).lineHeight)
    }));
    expect(await lines.locator(".gitPatchLine").count()).toBeLessThanOrEqual(Math.ceil(geometry.height / geometry.row) * 3);
    await lines.evaluate((node) => { node.scrollLeft = node.scrollWidth; });
    expect(await lines.evaluate((node) => node.scrollLeft)).toBeGreaterThan(0);
    await panel.getByRole("button", {name: "Restore diff viewer"}).click();
    await expect(panel).not.toHaveAttribute("data-fullscreen");
    await panel.getByRole("button", {name: "Show changed files"}).click();
    await expect(panel.locator(".gitFileList")).toBeVisible();
    await expect(panel.getByRole("button", {name: filename, exact: false})).toHaveAttribute("aria-current", "true");
    expect(queries).toBe(queryCount);
    await panel.getByRole("button", {name: "Back to Git tools"}).click();
    await expect(panel).toHaveAttribute("role", "complementary");
    await expect(panel).not.toHaveAttribute("data-fullscreen");
    await expect(panel.getByRole("button", {name: /Changes/})).toBeVisible();
  });
}

test("Git tools commit and push directly without a Turn or consuming the composer draft", async ({page}) => {
  await createSession(page);
  const remote = path.join(dataDir, "git-remote.git");
  execFileSync("git", ["init", "--bare", remote]);
  execFileSync("git", ["remote", "add", "origin", remote], {cwd: workspaceDir});
  await writeFile(path.join(workspaceDir, "README.md"), "# Visual fixture\n\nReviewed change.\n");
  execFileSync("git", ["add", "README.md"], {cwd: workspaceDir});
  await writeFile(path.join(workspaceDir, "not-selected.txt"), "Do not stage by default\n");
  const originalHead = execFileSync("git", ["rev-parse", "HEAD"], {cwd: workspaceDir, encoding: "utf8"});
  let turns = 0;
  let submission: {action: string; paths: string[]; revision: string} | undefined;
  page.on("request", (request) => {
    if (request.url().endsWith("/operation/submit")) turns++;
    if (request.url().endsWith("/workspace/git-action")) submission = request.postDataJSON();
  });
  await page.getByPlaceholder("Ask QCode").fill("Unfinished question");
  await page.getByRole("button", {name: "Refresh Git"}).click();
  await page.getByRole("button", {name: "Commit or push", exact: true}).click();
  const dialog = page.getByRole("dialog", {name: "Commit or push"});
  await expect(dialog.getByLabel("Commit message")).toBeFocused();
  await expect(dialog.getByRole("checkbox")).not.toBeChecked();
  await expect(dialog.getByRole("button", {name: "Commit", exact: true})).toBeDisabled();
  await dialog.getByLabel("Commit message").fill("reviewed change");
  await expect(dialog.getByRole("button", {name: "Push", exact: true})).toBeEnabled();
  await dialog.screenshot({path: path.join(repositoryRoot, ".tmp/git-tools-commit.png")});
  await dialog.getByRole("button", {name: "Commit and push", exact: true}).click();
  await expect(page.getByRole("status")).toContainText("Committed");
  await expect(page.getByRole("status")).toContainText("Pushed to origin/");
  expect(submission).toMatchObject({
    action: "commit_push", paths: ["README.md"], include_unstaged: false, message: "reviewed change"
  });
  expect(submission?.revision).toBeTruthy();
  expect(turns).toBe(0);
  const head = execFileSync("git", ["rev-parse", "HEAD"], {cwd: workspaceDir, encoding: "utf8"}).trim();
  const branch = execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim();
  expect(head).not.toBe(originalHead.trim());
  expect(execFileSync("git", ["--git-dir", remote, "rev-parse", `refs/heads/${branch}`], {encoding: "utf8"}).trim()).toBe(head);
  expect(execFileSync("git", ["ls-tree", "--name-only", "HEAD"], {cwd: workspaceDir, encoding: "utf8"})).not.toContain("not-selected");
  await expect(page.locator(".assistantMessage")).toHaveCount(0);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/git-direct-result.png")});
  await page.getByRole("button", {name: "Close Git tools"}).click();
  await expect(page.getByPlaceholder("Ask QCode")).toHaveValue("Unfinished question");
});

test("Git tools create a local branch without a Session or model Turn", async ({page}) => {
  let turns = 0;
  page.on("request", (request) => {
    if (request.url().endsWith("/operation/submit")) turns++;
  });
  const branch = execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim();
  const panel = page.getByRole("complementary", {name: "Git tools"});
  await panel.getByRole("button", {name: branch, exact: true}).click();
  await panel.getByRole("textbox", {name: "New Git branch name"}).fill("direct-feature");
  await panel.getByRole("button", {name: "Create", exact: true}).click();
  await expect(panel.getByRole("status")).toContainText("Switched to direct-feature");
  expect(execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim()).toBe("direct-feature");
  expect(turns).toBe(0);
  await expect(page.locator(".sessionRow")).toHaveCount(0);
});

test("Git tools preserve mobile focus and dark-theme geometry", async ({page}) => {
  await createSession(page);
  await writeFile(path.join(workspaceDir, "README.md"), "# Visual fixture\n\nNew text.\n");
  await page.setViewportSize({width: 390, height: 844});
  await page.emulateMedia({colorScheme: "dark", reducedMotion: "no-preference"});
  const trigger = page.getByRole("button", {name: "Git tools", exact: true});
  const panel = page.locator("#git-tools");
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAttribute("role", "complementary");
  await page.getByPlaceholder("Ask QCode").fill("Mobile draft");
  await page.keyboard.press("Escape");
  await expect(panel).toBeVisible();
  await expect(page.getByPlaceholder("Ask QCode")).toBeFocused();
  await expect(panel.getByRole("button", {name: /Changes/})).toBeVisible();
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/git-default-mobile-summary.png")});
  await panel.getByRole("button", {name: "Refresh Git"}).click();
  await panel.getByRole("button", {name: "Commit or push", exact: true}).click();
  await expect(page.getByLabel("Commit message")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(panel.getByRole("button", {name: "Commit or push", exact: true})).toBeFocused();
  await panel.getByRole("button", {name: /Changes/}).click();
  await expect(panel).toHaveAttribute("role", "dialog");
  await panel.getByRole("button", {name: /README.md/}).click();
  await expect(panel.getByLabel("Git file diff")).toContainText("New text.");
  await assertViewportGeometry(page);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/git-tools-mobile-dark.png")});
  await page.emulateMedia({reducedMotion: "reduce"});
  await page.keyboard.press("Escape");
  await expect(trigger).toBeFocused();
  await expect(panel).toHaveCount(0);
  await expect(page.locator(".conversation")).not.toHaveAttribute("inert", "");
  await expect(page.getByPlaceholder("Ask QCode")).toHaveValue("Mobile draft");
});

test("Git tools default visibility survives reload and respects dismissal within a Workspace", async ({page}) => {
  const panel = page.getByRole("complementary", {name: "Git tools"});
  await expect(panel).toBeVisible();
  await panel.getByRole("button", {name: "Close Git tools"}).click();
  await createSession(page);
  await expect(panel).toHaveCount(0);
  await page.getByRole("button", {name: "Git tools", exact: true}).click();
  await expect(panel).toBeVisible();
  await page.reload();
  await expect(panel).toBeVisible();
  await expect(panel.getByRole("button", {name: /Changes/})).toBeVisible();
  await expect(page.locator(".workspaceGroup select")).toHaveCount(0);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/git-default-desktop.png")});
});

test("captures empty and settings states", async ({page}) => {
  await expect(page).toHaveScreenshot("canonical-empty.png");

  await page.getByRole("button", {name: "Settings"}).click();
  await expect(page.getByRole("dialog", {name: "Settings"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-settings.png");

});

test("captures populated model, tool, and agent settings", async ({page}) => {
  await createSession(page);
  await page.getByRole("button", {name: "Settings"}).click();
  await page.getByRole("button", {name: "Models"}).click();
  await expect(page.getByRole("button", {name: "Test connection"})).toBeVisible();
  await expect(page.getByText("Runtime-managed")).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-settings-connection.png");

  await page.getByRole("button", {name: "Models"}).click();
  await expect(page.getByText("Context window")).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-settings-models.png");

  await page.getByRole("button", {name: "Tools", exact: true}).click();
  await expect(page.getByRole("searchbox", {name: "Search tools"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-settings-tools.png");

  await page.getByRole("button", {name: "Agent preset"}).click();
  await expect(page.getByLabel("Agent mode")).toHaveCount(0);
  await expect(page).toHaveScreenshot("canonical-settings-agent.png");
});

test("captures a blank session with the centered composer", async ({page}) => {
  await createSession(page);
  await expect(page.getByLabel("Session details")).toHaveCount(0);
  await expect(page).toHaveScreenshot("canonical-blank-session.png");
});

test("Material drawers preserve focus and mobile session access", async ({page}) => {
  await page.getByRole("button", {name: "Collapse sidebar", exact: true}).click();
  await page.setViewportSize({width: 390, height: 844});
  const menu = page.getByRole("button", {name: "Open session drawer", exact: true});
  await menu.click();
  const rail = page.getByRole("dialog", {name: "Sessions", exact: true});
  await expect(rail).toBeVisible();
  await expect(rail.getByRole("button", {name: "Add workspace"})).toContainText("Add workspace");
  await expect(rail.locator(".workspaceHeader[data-active] .workspaceCreateAction button"))
    .toHaveAttribute("aria-label", /New session in/);
  await expect(rail.locator(".newSessionRow")).toHaveCount(0);
  await page.getByRole("button", {name: "Settings", exact: true}).click();
  await expect(page.getByRole("dialog", {name: "Settings", exact: true})).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(rail).toBeVisible();
  await expect(page.getByRole("button", {name: "Settings", exact: true})).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(rail).toHaveCount(0);
  await expect(menu).toBeFocused();
  await createSession(page);
  await expect(page.getByPlaceholder("Ask QCode")).toBeVisible();
  await expect(page.locator(".sessionRail")).not.toBeVisible();

  await expect(page.getByRole("button", {name: "Session inspector", exact: true})).toHaveCount(0);
  await expect(page.getByRole("button", {name: "Export session", exact: true})).toHaveCount(0);
  await expect(page.locator(".conversation")).not.toHaveAttribute("inert", "");
  await assertViewportGeometry(page);
  await submitPrompt(page, "visual commentary");
  await expect(page.locator(".assistantMessage:not(.commentaryMessage)").last())
    .toContainText("Inspected the workspace with focused tools.");
  await page.getByRole("button", {name: "Search conversation", exact: true}).click();
  await page.getByRole("combobox", {name: "Search conversation"}).fill("handler is present");
  await page.getByRole("option", {name: /The handler is present/}).click();
  await expect(page.locator('[data-entry-kind="commentary"][data-navigation-current]'))
    .toContainText("The handler is present.");
  await assertViewportGeometry(page);
});

test("Material simplified chrome preserves file previews and responsive Prefix metrics", async ({page}) => {
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual diff");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Updated README and verified the diff.");
  await expect(page.getByRole("button", {name: "Session inspector", exact: true})).toHaveCount(0);
  await expect(page.getByRole("button", {name: "Export session", exact: true})).toHaveCount(0);
  await page.getByRole("button", {name: /Execution details/}).click();
  const edit = page.locator(".toolDisclosure").filter({hasText: "Edit"}).first();
  await edit.getByRole("button", {name: /^Edit/}).click();
  await expect(edit.locator("[data-diff]")).toContainText("README.md");
  await expect(edit.getByRole("button", {name: "README.md", exact: true})).toHaveCount(0);
  await page.getByPlaceholder("Ask QCode").fill("/export");
  await expect(page.getByRole("menuitem", {name: /export/i})).toHaveCount(0);
  await page.getByPlaceholder("Ask QCode").fill("");

  // Use a stable large prefix measurement to exercise the toolbar on every viewport.
  await page.route("**/api/v1/session/snapshot", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const sample = body.result.events.find((event: {kind: string}) => event.kind === "provider.attempt");
    expect(sample).toBeTruthy();
    sample.kind = "usage";
    sample.data = {
      sample: 1, input_tokens: 60000, output_tokens: 100, cached_tokens: 57430,
      context: {prefix_compared: true, prefix_common_tokens: 57430}
    };
    await route.fulfill({response, json: body});
  });
  await page.reload();
  await page.getByRole("button", {name: "Trajectory", exact: true}).click();
  const metrics = page.getByLabel("Prefix metrics");
  await expect(metrics).toContainText("57.4K");
  await expect(metrics).toHaveAttribute("title", "57,430 prefix tokens");
  for (const [width, height, colorScheme] of [
    [1440, 900, "light"], [1024, 768, "dark"], [390, 844, "light"]
  ] as const) {
    await page.setViewportSize({width, height});
    await page.emulateMedia({colorScheme});
    await assertViewportGeometry(page, ".conversationHeader button, .trajectoryToolbar button, .composer button");
    const search = await page.getByRole("searchbox", {name: "Search trajectory"}).boundingBox();
    const count = await metrics.boundingBox();
    expect(search && count && search.x + search.width <= count.x).toBeTruthy();
    await expect(page).toHaveScreenshot(`simplified-trajectory-${width}-${colorScheme}.png`);
  }
  const accessibility = await new AxeBuilder({page})
    .include(".trajectoryToolbar").withTags(["wcag2a", "wcag2aa"]).analyze();
  expect(accessibility.violations).toEqual([]);
});

test("Material motion supports rapid disclosure toggles and reduced motion", async ({page}) => {
  await page.emulateMedia({reducedMotion: "no-preference"});
  await createSession(page);
  await submitPrompt(page, "visual commentary");
  await expect(page.locator(".assistantMessage:not(.commentaryMessage)").last())
    .toContainText("Inspected the workspace with focused tools.");
  const toggle = page.getByRole("button", {name: /Execution details/});
  await toggle.click();
  await expect.poll(() => page.locator(".turnExecution > .uiCollapse").first().evaluate((node) =>
    node.getAnimations().some((animation) => animation.playState === "running")
  )).toBe(true);
  await expect(page.locator(".commentaryMessage")).toHaveCount(2);
  await toggle.click();
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(page.locator(".commentaryMessage").last()).toBeVisible();
  await toggle.click();
  await expect(page.locator(".commentaryMessage")).toHaveCount(0);
  await page.emulateMedia({reducedMotion: "reduce"});
  await toggle.click();
  await expect(page.locator(".commentaryMessage")).toHaveCount(2);
  await expect.poll(() => page.evaluate(() =>
    document.getAnimations().filter((animation) => animation.playState === "running").length
  )).toBe(0);
  await toggle.click();
  await expect(page.locator(".commentaryMessage")).toHaveCount(0);
});

test("Material nested model dialog preserves autofocus and keyboard ownership", async ({page}) => {
  await createSession(page);
  await page.getByRole("button", {name: "Settings", exact: true}).click();
  await page.getByRole("button", {name: "Models", exact: true}).click();
  await page.getByRole("button", {name: "Add model", exact: true}).click();
  await expect(page.getByRole("textbox", {name: "New model ID"})).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("textbox", {name: "New model ID"})).toHaveCount(0);
  await expect(page.getByRole("dialog", {name: "Settings", exact: true})).toBeVisible();
  await expect(page.getByRole("button", {name: "Add model", exact: true})).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", {name: "Settings", exact: true})).toHaveCount(0);
  await expect(page.getByRole("button", {name: "Settings", exact: true})).toBeFocused();
  await expect(page.locator(".conversation")).not.toHaveAttribute("inert", "");
});

test("Motion dialogs retain exit frames and restore nested focus before unmount", async ({page}) => {
  await page.emulateMedia({reducedMotion: "no-preference"});
  await createSession(page);
  const settings = page.getByRole("button", {name: "Settings", exact: true});
  await settings.click();
  await page.getByRole("button", {name: "Models", exact: true}).click();
  const add = page.getByRole("button", {name: "Add model", exact: true});
  await add.click();
  const input = page.getByRole("textbox", {name: "New model ID"});
  await input.fill("unsaved-model");
  const original = await input.elementHandle();
  await page.keyboard.press("Escape");
  await expect(add).toBeFocused();
  await expect(page.locator('[data-exiting] .modelEditorDialog')).toHaveCount(1);
  await expect(input).toHaveCount(0);
  await add.click();
  expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
  await expect(input).toHaveValue("unsaved-model");
  await page.keyboard.press("Escape");
  await expect(page.locator(".modelEditorDialog")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(settings).toBeFocused();
  await expect(page.locator('[data-exiting] .settingsDialog')).toHaveCount(1);
  await expect(page.locator(".conversation")).not.toHaveAttribute("inert", "");
  await expect(page.locator(".settingsDialog")).toHaveCount(0);
});

test("Motion menus can reopen mid-exit and hand focus to another dialog", async ({page}) => {
  await page.emulateMedia({reducedMotion: "no-preference"});
  await createSession(page);
  const trigger = page.getByRole("button", {name: "Commands", exact: true});
  await trigger.click();
  const menu = await page.locator(".commandMenu").elementHandle();
  await page.keyboard.press("Escape");
  await expect(trigger).toBeFocused();
  await expect(page.locator('[data-exiting] .commandMenu')).toHaveCount(1);
  await trigger.click();
  expect(await menu!.evaluate((node) => node.isConnected)).toBe(true);
  await page.getByRole("menuitem", {name: /context/}).click();
  await expect(page.getByRole("dialog", {name: "Add context"})).toBeVisible();
  await expect(page.locator(".conversation")).toHaveAttribute("inert", "");
  await page.keyboard.press("Escape");
  await expect(trigger).toBeFocused();
  await expect(page.locator(".contextDialog")).toHaveCount(0);
  await expect(page.locator(".commandMenu")).toHaveCount(0);
  await page.getByPlaceholder("Ask QCode").fill("/context");
  await expect(page.getByRole("searchbox", {name: "Search commands"})).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog", {name: "Add context"})).toBeVisible();
  await expect(page.getByRole("button", {name: "Close context browser"})).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByPlaceholder("Ask QCode")).toBeFocused();
});

test("Motion session drawer releases interaction during exit", async ({page}) => {
  await page.emulateMedia({reducedMotion: "no-preference"});
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual diff");
  await expect(page.locator(".assistantMessage").last()).toContainText("Updated");
  await page.setViewportSize({width: 390, height: 844});
  await page.getByRole("button", {name: "Open session drawer"}).click();
  await expect(page.locator(".workspaceHeader[data-active] .workspaceCreateAction button")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".sessionRail")).toHaveAttribute("inert", "");
  await expect(page.locator(".sessionRail .sessionList")).toBeAttached();
  await page.emulateMedia({reducedMotion: "reduce"});
  await expect(page.locator(".sessionRail")).not.toBeVisible();
  await expect(page.locator("[data-exiting]")).toHaveCount(0);
  await assertViewportGeometry(page);
});

test("Motion loading skeleton respects reduced motion without delaying content", async ({page}) => {
  await page.emulateMedia({reducedMotion: "no-preference"});
  await createSession(page);
  await submitPrompt(page, "visual commentary");
  await expect(page.locator(".assistantMessage:not(.commentaryMessage)").last())
    .toContainText("Inspected the workspace");
  let release!: () => void;
  const loaded = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/assets/Trajectory-*.js", async (route) => {
    await loaded;
    await route.continue();
  });
  await page.getByRole("button", {name: "Trajectory", exact: true}).click();
  const skeleton = page.getByRole("status", {name: "Loading trajectory"});
  await expect(skeleton).toBeVisible();
  expect(await skeleton.evaluate((node) =>
    node.getAnimations({subtree: true}).some((animation) => animation.playState === "running")
  )).toBe(true);
  await page.emulateMedia({reducedMotion: "reduce"});
  await expect.poll(() => skeleton.evaluate((node) => node.getAnimations({subtree: true}).length)).toBe(0);
  release();
  await expect(page.getByLabel("Execution trajectory")).toBeVisible();
  await expect(skeleton).toHaveCount(0);
});

test("preserves commentary across completion, reload, search and mobile layouts", async ({page}) => {
  await page.goto(new URL("/", baseURL).toString());
  await page.getByRole("button", {name: "Choose workspace", exact: true}).click();
  await page.getByRole("dialog", {name: "Workspaces"})
    .locator(".workspaceRow").filter({hasText: path.basename(workspaceDir)}).click();
  await createSession(page);
  await submitPrompt(page, "visual commentary");
  const final = page.locator(".assistantMessage:not(.commentaryMessage)").last();
  await expect(final).toContainText("Inspected the workspace with focused tools.");
  await expect(page.locator(".commentaryMessage")).toHaveCount(0);
  await page.getByRole("button", {name: /Execution details/}).click();
  await expect(page.locator(".commentaryMessage")).toHaveCount(2);
  const order = await page.locator(".turnExecutionItems [data-entry-kind]").evaluateAll(
    (nodes) => nodes.map((node) => node.getAttribute("data-entry-kind"))
  );
  expect(order.filter((kind) => kind === "commentary" || kind === "tool"))
    .toEqual(["commentary", "tool", "commentary", "tool"]);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/commentary-desktop.png")});

  await page.reload();
  await expect(final).toContainText("Inspected the workspace with focused tools.");
  await page.getByRole("button", {name: "Search conversation", exact: true}).click();
  await page.getByRole("combobox", {name: "Search conversation"}).fill("handler is present");
  await page.getByRole("option", {name: /The handler is present/}).click();
  await expect(page.locator(".commentaryMessage")).toHaveCount(2);
  await expect(page.locator('[data-entry-kind="commentary"][data-navigation-current]'))
    .toContainText("The handler is present.");
  await page.setViewportSize({width: 390, height: 844});
  await expect(page.locator(".commentaryMessage").last()).toBeVisible();
  const overflow = await page.locator(".commentaryMessage").evaluateAll((nodes) =>
    nodes.some((node) => node.scrollWidth > node.clientWidth)
  );
  expect(overflow).toBe(false);
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/commentary-mobile.png")});
});

test("summarizes the session title once and preserves a manual rename", async ({page}) => {
  await page.goto(new URL("/", baseURL).toString());
  await page.getByRole("button", {name: "Choose workspace", exact: true}).click();
  await page.getByRole("dialog", {name: "Workspaces"})
    .locator(".workspaceRow").filter({hasText: path.basename(workspaceDir)}).click();
  await createSession(page);
  await submitPrompt(page, "visual session title: please improve how session names summarize the actual task instead of truncating its prompt");
  await expect(page.locator(".sessionTitle").last()).toHaveText("Improve session title summaries");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Inspected the workspace with focused tools.");
  const restoredSnapshot = page.waitForResponse((response) =>
    new URL(response.url()).pathname.endsWith("/api/v1/session/snapshot")
  );
  await page.reload();
  const restored = await (await restoredSnapshot).json();
  const events = restored.result.events as Array<{
    kind: string; sequence: number; data: {title_source?: string};
  }>;
  const titleEvent = events.find((event) => event.kind === "session.title.updated");
  const completed = events.find((event) => event.kind === "turn.completed");
  expect(titleEvent).toBeDefined();
  expect(completed).toBeDefined();
  expect(titleEvent!.sequence).toBeLessThan(completed!.sequence);
  expect(titleEvent!.data.title_source).toBe("auto");
  expect(events.some((event) => event.data.title_source === "temporary")).toBe(false);
  await expect(page.getByText("Continuing this turn", {exact: true})).toHaveCount(0);
  await expect(page.locator(".sessionTitle").last()).toHaveText("Improve session title summaries");
  await page.locator(".sessionRow").filter({hasText: "Improve session title summaries"}).hover();
  await page.getByRole("button", {name: "Session actions for Improve session title summaries"}).click();
  await page.getByRole("menuitem", {name: "Rename", exact: true}).click();
  const renameDialog = page.getByRole("dialog", {name: "Rename session"});
  await expect(renameDialog).toBeVisible();
  await renameDialog.getByLabel("Session title").fill("My investigation");
  await renameDialog.getByRole("button", {name: "Rename"}).click();
  await expect(page.locator(".sessionTitle").last()).toHaveText("My investigation");
  await submitPrompt(page, "visual session title: now investigate a different topic");
  await expect(page.locator(".assistantMessage")).toHaveCount(2);
  await expect(page.locator(".sessionTitle").last()).toHaveText("My investigation");
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/session-title-desktop.png")});
  await page.setViewportSize({width: 390, height: 844});
  await expect(page.locator(".conversationHeader h1")).toHaveText("My investigation");
  await page.screenshot({path: path.join(repositoryRoot, ".tmp/session-title-mobile.png")});
});

test("captures a durable image in the user message", async ({page}) => {
  await createSession(page);
  await page.locator('input[type="file"][aria-label="Attach files"]')
    .setInputFiles(path.join(repositoryRoot, "web/public/icon-192.png"));
  await expect(page.getByLabel("Composer attachments")).toContainText("PNG");
  await submitPrompt(page, "Describe this image");

  const image = page.getByRole("img", {name: "icon-192.png"});
  await expect(image).toBeVisible();
  await expect(page.locator(".userMessage").first())
    .toHaveScreenshot("canonical-user-image.png");
  await page.reload();
  await expect(page.getByRole("img", {name: "icon-192.png"})).toBeVisible();
});

test("captures the Workspace row without a duplicate branch control", async ({page}) => {
  const workspace = page.locator(".workspaceGroup").first();
  await expect(workspace.getByRole("combobox")).toHaveCount(0);
  await expect(workspace).toHaveScreenshot("canonical-workspace-branch.png");
});

test("captures a branch conflict in Git tools", async ({page}) => {
  execFileSync("git", ["branch", "feature"], {cwd: workspaceDir});
  execFileSync("git", ["switch", "feature"], {cwd: workspaceDir});
  await writeFile(path.join(workspaceDir, "README.md"), "# Feature\n");
  execFileSync("git", ["add", "README.md"], {cwd: workspaceDir});
  execFileSync("git", [
    "-c", "user.name=QCode",
    "-c", "user.email=fixture@qcode.invalid",
    "commit", "-qm", "feature change"
  ], {cwd: workspaceDir});
  execFileSync("git", ["switch", "-"], {cwd: workspaceDir});
  await writeFile(path.join(workspaceDir, "README.md"), "# Local changes\n");
  await page.reload();
  const panel = page.getByRole("complementary", {name: "Git tools"});
  const branch = execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim();
  await panel.getByRole("button", {name: branch, exact: true}).click();
  await panel.getByRole("button", {name: "feature", exact: true}).click();

  const alert = page.getByRole("alert");
  await expect(alert).toContainText("Branch not switched");
  expect(execFileSync("git", ["branch", "--show-current"], {cwd: workspaceDir, encoding: "utf8"}).trim()).toBe(branch);
  expect(execFileSync("git", ["diff", "--", "README.md"], {cwd: workspaceDir, encoding: "utf8"})).toContain("+# Local changes");
  await expect(page).toHaveScreenshot("canonical-error-notice.png");
});

test("captures the modal workspace context browser", async ({page}) => {
  await createSession(page);
  await page.getByRole("button", {name: "Commands"}).click();
  await page.getByRole("menuitem", {name: /context/}).click();
  await expect(page.getByRole("dialog", {name: "Add context"})).toBeVisible();
  await expect(page.getByRole("button", {name: /README.md/})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-context-browser.png");
});

test("captures the authoritative diff state", async ({page}) => {
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual diff");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Updated README and verified the diff.");
  await expect(page.getByText("Completed", {exact: true})).toHaveCount(0);
  await page.getByRole("button", {name: /Execution details/}).click();
  const edit = page.locator('.toolDisclosure[data-variant="diff"]');
  await edit.locator(".disclosureLeading").click();
  await expect(edit.locator("[data-diff]")).toContainText("README.md");
  await expect(edit.locator(".diffFooter")).toContainText("+2 -1");
  await expect(page.getByRole("region", {name: "Produced files"})).toHaveCount(0);
  await expect(page.getByRole("complementary", {name: "Git tools"}).getByRole("button", {name: /Changes/})).toContainText("1 files");
  await expect(page).toHaveScreenshot("canonical-diff.png");
  await page.setViewportSize({width: 390, height: 844});
  await expect(page.getByRole("region", {name: "Produced files"})).toHaveCount(0);
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth
  )).toBe(true);
  await edit.getByRole("button", {name: "Inspect", exact: true}).click();
  await expect(page.getByRole("button", {name: "Trajectory"}))
    .toHaveAttribute("aria-current", "page");
});

test("captures collapsed tools, expanded tool detail, and trajectory", async ({page}) => {
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual diff");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Updated README and verified the diff.");
  await expect(page.getByLabel("Session details")).toHaveCount(0);
  await page.getByRole("button", {name: /Execution details/}).click();

  const tool = page.locator(".toolDisclosure").first();
  await expect(tool).toHaveAttribute("data-call-id", /.+/);
  await expect(tool.locator("pre")).toHaveCount(0);
  await expect(tool.locator(".disclosureChevron")).toHaveCSS("opacity", "0");
  await expect(tool.locator(":scope > .disclosureRow")).not.toContainText("completed");
  await expect(page).toHaveScreenshot("canonical-tool-collapsed.png");

  await tool.locator(":scope > .disclosureRow").hover();
  await expect(tool.locator(".disclosureChevron")).toHaveCSS("opacity", "1");
  await tool.locator(":scope > .disclosureRow .disclosureLeading").click();
  await expect(tool.locator(
    ".toolIOCard, [data-read], [data-terminal], [data-search], [data-diff]"
  )).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-tool-expanded.png");

  await tool.getByRole("button", {name: "Inspect"}).click();
  await expect(page.getByLabel("Execution trajectory")).toBeVisible();
  const inspector = page.getByRole("complementary", {name: "Record inspector"});
  await expect(inspector).toBeVisible();
  await expect(inspector.getByRole("tab", {name: "Summary"}))
    .toHaveAttribute("aria-selected", "true");
  await expect(inspector.getByRole("tab", {name: "Input"})).toBeVisible();
  await expect(inspector.getByRole("tab", {name: "Output"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-trajectory.png");
  await inspector.getByRole("tab", {name: "Output"}).click();
  await expect(inspector.getByRole("button", {name: "Copy output"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-trajectory-detail.png");
});

test("captures Think and specialized Read, Bash, Grep, and Glob cards", async ({page}) => {
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual tools");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Inspected the workspace with focused tools.");
  await page.getByRole("button", {name: /Execution details/}).click();

  const think = page.locator(".reasoningDisclosure");
  await expect(think).toContainText("Think");
  await expect(think).toContainText("I will inspect the file");

  const read = page.locator('.toolDisclosure[data-variant="read"]').first();
  await read.locator(".disclosureLeading").click();
  await expect(read.locator("[data-read]")).toBeVisible();
  await read.scrollIntoViewIfNeeded();
  await expect(page).toHaveScreenshot("canonical-think-read.png");

  const bash = page.locator('.toolDisclosure[data-variant="shell"]').first();
  await bash.locator(".disclosureRow").click();
  await expect(bash.locator("[data-terminal]")).toBeVisible();
  await bash.scrollIntoViewIfNeeded();
  await expect(page).toHaveScreenshot("canonical-bash.png");

  const searches = page.locator('.toolDisclosure[data-variant="search"]');
  await searches.nth(0).locator(".disclosureRow").click();
  await searches.nth(1).locator(".disclosureRow").click();
  await expect(searches.nth(0).locator("[data-search]")).toBeVisible();
  await expect(searches.nth(1).locator("[data-search]")).toBeVisible();
  await searches.nth(0).scrollIntoViewIfNeeded();
  await expect(page).toHaveScreenshot("canonical-search.png");
});

test("captures the back-to-bottom control at the transcript edge", async ({page}) => {
  await page.setViewportSize({width: 1024, height: 600});
  await createSession(page);
  await page.getByLabel("Approval").selectOption("auto");
  await submitPrompt(page, "visual tools");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Inspected the workspace with focused tools.");
  await page.getByRole("button", {name: /Execution details/}).click();
  for (const row of await page.locator(".toolDisclosure .disclosureRow").all()) {
    await row.click();
  }
  await page.locator(".conversationScrollport").evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event("scroll"));
  });
  const button = page.getByRole("button", {name: "Back to bottom"});
  await expect(button).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-back-to-bottom.png");
  await button.click();
  await expect(button).toHaveCount(0);
});

test("captures streaming and completed states", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual streaming");
  await expect(page.getByText("Working", {exact: true})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-streaming.png");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");
  await expect(page.getByText("Completed", {exact: true})).toHaveCount(0);
  await expect(page).toHaveScreenshot("canonical-completed.png");
});

test("steers the active turn without hiding the stop action", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual long streaming");
  await expect(page.getByRole("button", {name: "Stop turn"})).toBeVisible();

  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill("Focus on the final verification");
  await page.getByRole("button", {name: "Steer current turn"}).click();

  const steering = page.locator(".userMessage[data-steering]");
  await expect(steering).toContainText("Focus on the final verification");
  await expect(composer).toHaveValue("");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");
});

test("queues a follow-up and advances it after the active turn", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual queue");
  await expect(page.getByRole("button", {name: "Stop turn"})).toBeVisible();

  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill("Verify the queued follow-up");
  await page.getByRole("button", {name: "Queue next"}).click();

  await expect(page.getByText("1 queued message")).toBeVisible();
  await expect(page.locator(".userMessage").filter({
    hasText: "Verify the queued follow-up"
  })).toBeVisible();
  await expect(page.getByText("1 queued message")).not.toBeVisible();
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");
});

test("captures message actions, commands, context usage, and rich Markdown", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual chrome");
  await expect(page.getByRole("heading", {name: "Core modules"})).toBeVisible();
  await expect(page.getByRole("table")).toBeVisible();
  await expect(page.getByRole("button", {name: "Copy response"})).toBeVisible();
  await expect(page.getByRole("button", {
    name: "Like response",
    exact: true
  })).toBeVisible();
  await expect(page.getByRole("button", {
    name: "Dislike response",
    exact: true
  })).toBeVisible();

  const selectWidths = await page.evaluate(() => ({
    approval: document.querySelector<HTMLSelectElement>('select[aria-label="Approval"]')
      ?.getBoundingClientRect().width ?? 0
  }));
  await expect(page.getByLabel("Mode", {exact: true})).toHaveCount(0);
  expect(selectWidths.approval).toBeLessThan(92);
  await expect(page).toHaveScreenshot("canonical-message-chrome.png");

  await page.getByRole("button", {name: "Commands"}).click();
  await expect(page.getByRole("menu", {name: "Commands"})).toBeVisible();
  await expect(page.getByRole("menuitem", {name: /compact/})).toBeEnabled();
  await expect(page).toHaveScreenshot("canonical-command-menu.png");
  await page.getByRole("button", {name: "Commands"}).click();

  const context = page.getByRole("button", {name: /of context used/});
  await expect(context).toBeVisible();
  await context.click();
  await expect(page.getByRole("dialog", {name: "Context usage"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-context-usage.png");

  await page.getByRole("button", {
    name: "Dislike response",
    exact: true
  }).click();
  await expect(page.getByRole("button", {name: "Remove dislike"}))
    .toHaveAttribute("aria-pressed", "true");
  await page.waitForTimeout(200);
  await page.reload();
  await expect(page.getByRole("button", {name: "Remove dislike"}))
    .toHaveAttribute("aria-pressed", "true");
});

test("renders rich Markdown without stretching the conversation", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual rich content");
  await expect(page.getByRole("heading", {name: "Rich content"})).toBeVisible();

  const message = page.locator(".assistantMessage").last();
  await expect(message.locator("strong")).toContainText("注意：");
  await expect(message.locator(".katex")).toHaveCount(2);
  await expect(message.locator('math[display="block"]')).toHaveCount(1);
  await expect(message.getByRole("button", {
    name: "Open file README.md"
  })).toHaveCount(0);
  await expect(message.locator('.markdownFileReference[title="README.md"]')).toBeVisible();

  const image = message.getByRole("img", {name: "QCode mark"});
  await image.scrollIntoViewIfNeeded();
  await expect.poll(() => image.evaluate(
    (element: HTMLImageElement) => element.naturalWidth
  )).toBeGreaterThan(0);
  await expect(message.getByRole("link", {
    name: "Download image QCode mark"
  })).toBeVisible();
  await expect(message.getByRole("alert")).toContainText("Image unavailable");
  await expect(message.getByRole("button", {
    name: "Retry image Missing diagram"
  })).toBeVisible();

  const table = message.getByRole("region", {name: "Response table"});
  const code = message.locator(".markdownCodeBlock pre");
  await expect(table).toBeVisible();
  await expect(code).toBeVisible();
  expect(await table.evaluate(
    (element) => element.scrollWidth > element.clientWidth
  )).toBe(true);
  expect(await code.evaluate(
    (element) => element.scrollWidth > element.clientWidth
  )).toBe(true);
  await expect(message.getByText("Nested item")).toBeVisible();

  const accessibility = await new AxeBuilder({page})
    .include(".assistantMessage")
    .withTags(["wcag2a", "wcag2aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);
  await page.setViewportSize({width: 1440, height: 1200});
  await page.locator(".conversationScrollport").evaluate((element) => {
    element.scrollTop = 0;
  });
  await expect(page).toHaveScreenshot("canonical-rich-content.png");

  await page.setViewportSize({width: 390, height: 844});
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth
  )).toBe(true);
});

test("navigates long conversations by stable semantic anchors", async ({page}) => {
  await page.setViewportSize({width: 1200, height: 760});
  await createSession(page);
  for (const suffix of ["first", "second", "third", "fourth"]) {
    const completed = page.locator(".assistantMessage");
    const before = await completed.count();
    await submitPrompt(page, `visual navigation ${suffix}`);
    await expect(completed).toHaveCount(before + 1);
  }

  await expect(page.getByRole("button", {name: "Search conversation"}))
    .toContainText("4/4");
  await page.getByRole("button", {name: "Search conversation"}).click();
  const navigator = page.getByRole("dialog", {name: "Search conversation"});
  await expect(navigator).toBeVisible();
  await expect(navigator.getByRole("option")).not.toHaveCount(0);
  const accessibility = await new AxeBuilder({page})
    .include(".conversationNavigator")
    .withTags(["wcag2a", "wcag2aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);
  await expect(page).toHaveScreenshot("canonical-conversation-navigator.png");

  await navigator.getByRole("tab", {name: "Files"}).click();
  await navigator.getByRole("combobox", {name: "Search conversation"})
    .fill("main.go");
  await navigator.getByRole("option").first().click();
  const selected = page.locator(
    ".transcriptEntryAnchor[data-navigation-current]"
  );
  await expect(selected.locator(".toolDisclosure")).toBeVisible();
  await expect(page.getByRole("button", {name: "Search conversation"}))
    .toContainText("1/4");

  await page.getByRole("button", {name: "Next user question"}).click();
  await expect(
    page.locator(".transcriptEntryAnchor[data-navigation-current] .userMessage")
  ).toContainText("visual navigation second");
  await expect(page.getByRole("button", {name: "Search conversation"}))
    .toContainText("2/4");
  await page.getByRole("button", {name: "Next user question"}).click();
  const highlightedThird = page.locator(
    ".transcriptEntryAnchor[data-navigation-current]"
  );
  await expect(highlightedThird.locator(".userMessage"))
    .toContainText("visual navigation third");
  const thirdQuestion = page.locator(".transcriptEntryAnchor")
    .filter({hasText: "visual navigation third"});

  const anchorBefore = await transcriptAnchorTop(thirdQuestion);
  await page.getByRole("button", {name: "Trajectory"}).click();
  await expect(page.getByLabel("Execution trajectory")).toBeVisible();
  await page.getByRole("button", {name: "Chat", exact: true}).click();
  await expect.poll(
    async () => Math.abs(
      (await transcriptAnchorTop(thirdQuestion)) - anchorBefore
    )
  ).toBeLessThanOrEqual(2);

  const beforeToolExpansion = await transcriptAnchorTop(thirdQuestion);
  const firstTool = page.locator(".toolDisclosure .disclosureRow").first();
  await firstTool.evaluate((button: HTMLButtonElement) => button.click());
  await expect(firstTool).toHaveAttribute("aria-expanded", "true");
  await expect.poll(
    async () => Math.abs(
      (await transcriptAnchorTop(thirdQuestion)) - beforeToolExpansion
    )
  ).toBeLessThanOrEqual(2);

  const beforeSessionSwitch = await transcriptAnchorTop(thirdQuestion);
  await page.locator(".workspaceHeader[data-active] .workspaceCreateAction button").click();
  await expect(page.locator(".sessionRow")).toHaveCount(2);
  await page.locator(".sessionSelect")
    .filter({hasText: "visual navigation first"})
    .click();
  await expect(thirdQuestion).toBeVisible();
  await expect.poll(
    async () => Math.abs(
      (await transcriptAnchorTop(thirdQuestion)) - beforeSessionSwitch
    )
  ).toBeLessThanOrEqual(2);

  await page.setViewportSize({width: 390, height: 844});
  await page.getByRole("button", {name: "Search conversation"}).click();
  await expect(page.getByRole("dialog", {name: "Search conversation"}))
    .toBeVisible();
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth
  )).toBe(true);
});

test("captures the approval state", async ({page}) => {
  await createSession(page);
  await enableAutomaticPlanApproval(page);
  await submitPrompt(page, "visual approval");
  await expect(page.getByText("exec_command requires approval")).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-approval.png");
});

test("captures the implementation plan", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual plan");

  const progress = page.getByRole("region", {name: "Session progress"});
  await expect(progress).toBeVisible();
  await expect(progress).toContainText("Stabilize prompt cache prefixes");
  await expect(progress.getByRole("button", {name: "Implement"})).toBeVisible();
  await expect(progress).not.toContainText('{"version":1');
  await expect(progress).toHaveScreenshot("canonical-plan.png");

  await page.setViewportSize({width: 390, height: 844});
  await progress.scrollIntoViewIfNeeded();
  expect(await progress.evaluate((node) =>
    node.scrollWidth <= node.clientWidth
  )).toBe(true);
  await expect(progress).toHaveScreenshot("canonical-plan-mobile.png");
});

test("captures a complex edit and approval workflow", async ({page}) => {
  await createSession(page);
  await enableAutomaticPlanApproval(page);
  await submitPrompt(page, "visual edit approval");

  await expect(page.getByText("Waiting for approval")).toBeVisible();
  await expect(page.getByText("exec_command requires approval")).toBeVisible();
  const pendingEdit = page.locator('.toolDisclosure[data-variant="diff"]');
  await pendingEdit.locator(".disclosureLeading").click();
  await expect(pendingEdit.locator("[data-diff]")).toContainText("README.md");
  await expect(page).toHaveScreenshot("canonical-edit-approval.png");

  await page.getByRole("button", {name: "Approve once"}).click();
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Updated");
  await page.getByRole("button", {name: /Execution details/}).click();
  await pendingEdit.locator(".disclosureLeading").click();
  await expect(pendingEdit.locator("[data-diff]")).toContainText("README.md");
  await expect(page).toHaveScreenshot("canonical-edit-completed.png");
});

test("captures the input state", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual input");
  await expect(page.getByText("Choose the verification scope")).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-input.png");
});

test("captures the failure state", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual failure");
  await expect(page.getByText("Failed", {exact: true})).toBeVisible();
  await expect(page.getByText("Review the workspace before continuing.")).toBeVisible();
  await expect(page.getByRole("button", {name: "Continue"})).toBeVisible();
  await expect(page).toHaveScreenshot("canonical-failure.png");
});

test("repeated reloads do not resubmit an active streaming turn", async ({page}) => {
  await createSession(page);
  await submitPrompt(page, "visual long streaming");
  await expect(page.getByText("Working", {exact: true})).toBeVisible();

  for (let index = 0; index < 5; index += 1) {
    await page.reload();
    await expect(page.locator(".app")).toBeVisible();
  }

  await expect(page.getByText("Connected", {exact: true})).toBeVisible();
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");
  await expect(page.locator(".userMessage", {
    hasText: "visual long streaming"
  })).toHaveCount(1);
});

test("a frozen tab converges after streaming completes", async ({page, context}) => {
  await createSession(page);
  await submitPrompt(page, "visual long streaming");
  await expect(page.getByText("Working", {exact: true})).toBeVisible();

  const session = await context.newCDPSession(page);
  await session.send("Page.setWebLifecycleState", {state: "frozen"});
  await new Promise((resolve) => setTimeout(resolve, 3_000));
  await session.send("Page.setWebLifecycleState", {state: "active"});
  await page.bringToFront();

  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");
  await expect(page.locator(".userMessage", {
    hasText: "visual long streaming"
  })).toHaveCount(1);
  await session.detach();
});

test("keeps background work visible and opens its completion notification", async ({page}) => {
  await page.addInitScript(() => {
    type CapturedNotification = {
      title: string;
      body: string;
      onclick: ((event: Event) => unknown) | null;
    };
    const captured: CapturedNotification[] = [];
    Object.assign(window, {__qcodeNotifications: captured});
    class TestNotification {
      static permission: NotificationPermission = "granted";
      static requestPermission = async () => "granted" as NotificationPermission;
      onclick: ((event: Event) => unknown) | null = null;
      onclose: ((event: Event) => unknown) | null = null;

      constructor(
        readonly title: string,
        readonly options?: NotificationOptions
      ) {
        captured.push({
          title,
          body: options?.body ?? "",
          onclick: (event) => this.onclick?.(event)
        });
      }

      close(): void {
        this.onclose?.(new Event("close"));
      }
    }
    Object.defineProperty(window, "Notification", {
      configurable: true,
      value: TestNotification
    });
  });
  await page.reload();
  await expect(page.locator(".app")).toBeVisible();

  await page.getByRole("button", {name: "Settings"}).click();
  const notifications = page.getByRole("switch", {name: "Desktop notifications"});
  await expect(notifications).not.toBeChecked();
  await notifications.check();
  await expect(notifications).toBeChecked();
  await page.getByRole("button", {name: "Close settings"}).click();

  await createSession(page);
  await submitPrompt(page, "visual long streaming");
  await expect(page.getByText("Working", {exact: true})).toBeVisible();
  await expect(page).toHaveTitle("(1) Working · QCode");
  await createSession(page);

  const background = page.locator(".sessionRow").filter({
    hasText: "visual long streaming"
  });
  await expect(background.locator('[title="Completed"]')).toBeVisible();
  const captured = await page.evaluate(() => (
    (window as unknown as {
      __qcodeNotifications: Array<{title: string; body: string}>;
    }).__qcodeNotifications
  ));
  expect(captured).toEqual([{
    title: "QCode task completed",
    body: "A background Session completed."
  }]);
  expect(JSON.stringify(captured)).not.toContain("visual long streaming");

  await page.evaluate(() => {
    const notification = (window as unknown as {
      __qcodeNotifications: Array<{
        onclick: ((event: Event) => unknown) | null;
      }>;
    }).__qcodeNotifications.at(-1);
    notification?.onclick?.(new Event("click"));
  });
  await expect(background).toHaveAttribute("data-active", "true");
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Review complete. Runtime evidence is consistent.");

  await createSession(page);
  await enableAutomaticPlanApproval(page);
  await submitPrompt(page, "visual background approval");
  await expect(page.getByText("Working", {exact: true})).toBeVisible();
  await createSession(page);
  const approvalBackground = page.locator(".sessionRow").filter({
    hasText: "visual background approval"
  });
  await expect(
    approvalBackground.locator('[title="Approval required"]')
  ).toBeVisible();
  await expect(page).toHaveTitle("(1) Action required · QCode");
  await expect.poll(() => page.evaluate(() => (
    (window as unknown as {
      __qcodeNotifications: Array<{title: string}>;
    }).__qcodeNotifications.length
  ))).toBe(2);
  const approvalNotice = await page.evaluate(() => (
    (window as unknown as {
      __qcodeNotifications: Array<{title: string; body: string}>;
    }).__qcodeNotifications.at(-1)
  ));
  expect(approvalNotice).toEqual({
    title: "QCode needs approval",
    body: "A background Session is waiting for approval."
  });
  await expect(page).toHaveScreenshot("canonical-background-activity.png");

  await page.evaluate(() => {
    const notification = (window as unknown as {
      __qcodeNotifications: Array<{
        onclick: ((event: Event) => unknown) | null;
      }>;
    }).__qcodeNotifications.at(-1);
    notification?.onclick?.(new Event("click"));
  });
  await expect(approvalBackground).toHaveAttribute("data-active", "true");
  await expect(page.getByText("Waiting for approval")).toBeVisible();
  await expect(page.getByLabel("Approval details")).toBeFocused();
  await page.getByRole("button", {name: "Approve once"}).click();
  await expect(page.locator(".assistantMessage").last())
    .toContainText("Approved command completed.");
});

test("captures viewport theme contrast and zoom matrix", async ({page}) => {
  for (const viewport of [
    {width: 390, height: 844},
    {width: 1024, height: 768},
    {width: 1440, height: 900},
    {width: 1920, height: 1080}
  ]) {
    for (const colorScheme of ["light", "dark"] as const) {
      await page.setViewportSize(viewport);
      await page.emulateMedia({
        colorScheme,
        forcedColors: "none",
        reducedMotion: "reduce"
      });
      await page.goto(baseURL);
      await assertViewportGeometry(page);
      await expect(page).toHaveScreenshot(
        `viewport-${viewport.width}x${viewport.height}-${colorScheme}.png`
      );
    }
  }

  await page.setViewportSize({width: 1024, height: 768});
  await page.emulateMedia({
    colorScheme: "light",
    forcedColors: "active",
    reducedMotion: "reduce"
  });
  await page.goto(baseURL);
  await assertViewportGeometry(page);
  await expect(page).toHaveScreenshot("viewport-1024x768-forced-colors.png");

  await page.emulateMedia({
    colorScheme: "light",
    forcedColors: "none",
    reducedMotion: "reduce"
  });
  await page.setViewportSize({width: 512, height: 384});
  await page.goto(baseURL);
  await assertViewportGeometry(page);
  await page.getByRole("button", {name: "Create session"}).scrollIntoViewIfNeeded();
  await expect(page.getByRole("button", {name: "Create session"})).toBeVisible();
  await expect(page).toHaveScreenshot("viewport-1024x768-zoom-200.png");
});

async function createSession(page: Page): Promise<void> {
  const sessions = page.locator(".sessionRow");
  const count = await sessions.count();
  const drawerToggle = page.getByRole("button", {name: "Open session drawer", exact: true});
  if (await drawerToggle.isVisible() && !await page.locator(".app").getAttribute("data-mobile-rail")) {
    await drawerToggle.click();
  }
  await page.locator(".workspaceHeader[data-active] .workspaceCreateAction button").click();
  await expect(sessions).toHaveCount(count + 1);
  await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
}

async function enableAutomaticPlanApproval(page: Page): Promise<void> {
  await page.getByRole("button", {name: "Settings"}).click();
  await page.getByRole("button", {name: "Agent preset"}).click();
  await page.getByLabel("Plan approval").selectOption("auto");
  await page.getByRole("button", {name: "Apply changes"}).click();
  await expect(page.getByText("Applied", {exact: true})).toBeVisible();
  await page.getByRole("button", {name: "Close settings"}).click();
}

async function submitPrompt(page: Page, prompt: string): Promise<void> {
  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill(prompt);
  await page.getByRole("button", {name: "Send"}).click();
}

async function assertViewportGeometry(page: Page, buttons = "button"): Promise<void> {
  await expect(page.locator(".app")).toBeVisible();
  const geometry = await page.evaluate((buttons) => {
    const app = document.querySelector<HTMLElement>(".app");
    const actions = Array.from(document.querySelectorAll<HTMLElement>(buttons))
      .filter((button) =>
        button.offsetParent !== null &&
        getComputedStyle(button).visibility === "visible" &&
        !button.closest('[inert], [aria-hidden="true"]')
      )
      .map((button) => button.getBoundingClientRect())
      .filter((box) => box.bottom > 0 && box.top < window.innerHeight);
    const primary = document.querySelector<HTMLElement>(
      ".composerSeat, .startupSetup"
    );
    const primaryBox = primary?.getBoundingClientRect();
    return {
      appOverflow: app ? app.scrollWidth - app.clientWidth : -1,
      actionsInside: actions.every(
        (box) =>
          box.left >= 0 &&
          box.right <= window.innerWidth &&
          box.top >= 0 &&
          box.bottom <= window.innerHeight
      ),
      primaryVisible: primaryBox
        ? primaryBox.top < window.innerHeight && primaryBox.bottom > 0
        : false
    };
  }, buttons);
  expect(geometry.appOverflow).toBeLessThanOrEqual(0);
  expect(geometry.actionsInside).toBe(true);
  expect(geometry.primaryVisible).toBe(true);
}

async function transcriptAnchorTop(
  anchor: Locator
): Promise<number> {
  return anchor.evaluate((element) => {
    const scrollport = element.closest<HTMLElement>("[data-conversation-scroll]");
    const content = element.firstElementChild;
    if (!scrollport || !(content instanceof HTMLElement)) {
      throw new Error("Conversation anchor is not mounted");
    }
    return content.getBoundingClientRect().top -
      scrollport.getBoundingClientRect().top;
  });
}

function runtimeURL(
  child: ChildProcessByStdio<null, Readable, Readable>
): Promise<string> {
  return new Promise((resolve, reject) => {
    let stdout = "";
    let stderr = "";
    let settled = false;
    const finish = (error?: Error, url?: string) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      child.stdout.off("data", onStdout);
      child.stderr.off("data", onStderr);
      child.off("exit", onExit);
      child.off("error", onError);
      if (error) reject(error);
      else resolve(url!);
    };
    const onStdout = (chunk: Buffer) => {
      stdout += chunk.toString();
      const match = stdout.match(/QCode Runtime Ready: (http:\/\/[^\s]+)/);
      if (match) void workspaceURL(match[1]).then(
        (url) => finish(undefined, url),
        (error: Error) => finish(error)
      );
    };
    const onStderr = (chunk: Buffer) => {
      stderr += chunk.toString();
    };
    const onExit = (code: number | null) => {
      finish(new Error(`Runtime exited before readiness (${code})\n${stderr}`));
    };
    const onError = (error: Error) => finish(error);
    const timeout = setTimeout(() => {
      finish(new Error(`Runtime readiness timed out\nstdout:\n${stdout}\nstderr:\n${stderr}`));
    }, 20_000);
    child.stdout.on("data", onStdout);
    child.stderr.on("data", onStderr);
    child.once("exit", onExit);
    child.once("error", onError);
  });
}

async function workspaceURL(origin: string): Promise<string> {
  const bootstrap = await fetch(new URL("/api/v1/bootstrap", origin));
  const value = await bootstrap.json() as {
    workspace_catalog: WorkspaceCatalog;
  };
  const workspaces = value.workspace_catalog.workspaces.filter((workspace) => workspace.ready);
  if (workspaces.length !== 1) {
    throw new Error("Expected one ready workspace in the isolated visual fixture");
  }
  const target = new URL(origin);
  target.searchParams.set("workspace", workspaces[0].id);
  return target.toString();
}
