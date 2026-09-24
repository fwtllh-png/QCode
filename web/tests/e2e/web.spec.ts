import {expect, test, type Page} from "@playwright/test";
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
const binary = process.env.QCODE_E2E_BINARY || path.join(repositoryRoot, "bin/qcode");

let server: ChildProcessByStdio<null, Readable, Readable>;
let dataDir: string;
let workspaceDir: string;
let baseURL: string;

test.describe.configure({mode: "serial"});

test.beforeEach(async () => {
  dataDir = await mkdtemp(path.join(tmpdir(), "qcode-web-e2e-"));
  workspaceDir = await mkdtemp(path.join(tmpdir(), "qcode-web-workspace-"));
  await writeFile(
    path.join(workspaceDir, "README.md"),
    "# Fixture workspace\n\nhello from the browser test\n"
  );
  await writeFile(
    path.join(workspaceDir, "main.go"),
    "package main\n\nfunc helloFixture() {}\n"
  );
  await writeFile(
    path.join(workspaceDir, "diagram.png"),
    Buffer.from(
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
      "base64"
    )
  );
  execFileSync("git", ["init", "-q"], {cwd: workspaceDir});
  server = spawn(
    binary,
    [
      "--workspace", workspaceDir,
      "--data-dir", dataDir,
      "--provider-fixture", path.join(repositoryRoot, "testdata/providers/openai"),
      "--provider", "openai",
      "--model", "fixture-model",
      "--enable-tools=false",
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

test("boots the real Runtime with an accessible empty state", async ({page}) => {
  await page.goto(baseURL);

  await expect(page.getByRole("heading", {name: "New Chat"})).toBeVisible();
  await expect(page.getByText("Connected", {exact: true})).toBeVisible();
  await expect(page.getByRole("button", {name: /New session in /})).toBeVisible();
  await expect(page.getByRole("button", {name: "Settings"})).toBeVisible();
  const searchSessions = page.getByRole("button", {name: "Search sessions"});
  await expect(searchSessions).toBeVisible();
  await searchSessions.click();
  await expect(page.getByRole("textbox", {name: "Search sessions"})).toBeFocused();
  await expect(page.getByRole("heading", {name: "Start a new session"})).toBeVisible();
  await expect(page.getByRole("button", {name: "Create session"})).toBeVisible();
  await expect(page.getByPlaceholder("Ask QCode")).toHaveCount(0);
  await expect(page.getByLabel("Session details")).toHaveCount(0);
  await expect(page.getByRole("button", {name: /detail panel/i})).toHaveCount(0);

});

test("requires Workspace selection on the bare Supervisor URL", async ({page}) => {
  await page.goto(new URL("/", baseURL).toString());

  await expect(page.getByRole("heading", {name: "Choose a workspace"}))
    .toBeVisible();
  await expect(page.getByRole("button", {name: "Choose workspace"}))
    .toBeVisible();
  await expect(page.getByRole("button", {name: /New session in /})).toHaveCount(0);
  await page.getByRole("button", {name: "Choose workspace"}).click();
  await expect(page.getByRole("dialog", {name: "Workspaces"})).toBeVisible();
});

// 能力，composer 不渲染 Reasoning 菜单；需为 fixture 补充能力元数据后恢复。
test("changes reasoning effort from an upward composer menu", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();

  const trigger = page.getByRole("button", {name: "Reasoning"});
  await expect(trigger).toContainText("Default");
  await trigger.click();

  const menu = page.getByRole("menu", {name: "Reasoning modes"});
  await expect(menu).toBeVisible();
  expect(await menu.getByRole("menuitemradio").allTextContents()).toEqual([
    "Default",
    "Low",
    "Medium",
    "High",
    "XHigh"
  ]);
  const desktopBounds = await menu.boundingBox();
  expect(desktopBounds).not.toBeNull();
  expect(desktopBounds!.y).toBeGreaterThanOrEqual(0);

  await menu.getByRole("menuitemradio", {name: "High", exact: true}).click();
  await expect(trigger).toContainText("High");

  await page.setViewportSize({width: 390, height: 844});
  await trigger.click();
  const mobileBounds = await menu.boundingBox();
  expect(mobileBounds).not.toBeNull();
  expect(mobileBounds!.x).toBeGreaterThanOrEqual(0);
  expect(mobileBounds!.x + mobileBounds!.width).toBeLessThanOrEqual(390);
  expect(mobileBounds!.y).toBeGreaterThanOrEqual(0);
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth
  )).toBe(true);
  const accessibility = await new AxeBuilder({page})
    .include(".reasoningMenuRoot")
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);
});

test("requires explicit connection fields during setup", async ({page}) => {
  await page.route("**/api/v1/bootstrap", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        protocol_version: 1,
        server_build: "setup-test",
        token: "setup-token",
        ready: false,
        draining: false,
        setup_required: true,
        workspace_root: workspaceDir
      })
    });
  });
  await page.route("**/api/v1/setup/probe", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        version: 1,
        result: {
          models: [{
            id: "deepseek-reasoner",
            context_tokens: 128000,
            max_output_tokens: 8192
          }],
          capabilities: {
            streaming: true,
            reasoning: true,
            reasoning_efforts: ["low", "high"],
            default_reasoning_effort: "high",
            tool_calls: true,
            native_search: false,
            incremental_responses: false,
            vision: false,
            image_input: false,
            prompt_cache: false,
            automatic_prompt_cache: false,
            thinking_toggle: false
          }
        }
      })
    });
  });
  await page.goto(baseURL);

  // setup 阶段不再阻断：主界面与引导横幅常显，配置入口在设置页。
  await expect(page.locator(".app")).toBeVisible();
  await expect(page.getByText(
    "Configure a model connection to start working with QCode."
  )).toBeVisible();
  await page.getByRole("button", {name: "Configure model", exact: true}).click();
  const dialog = page.getByRole("dialog", {name: "Settings"});
  await expect(dialog).toBeVisible();
  await expect(page.getByRole("button", {name: "Save and start"})).toBeDisabled();

  // 四要素向导：Base URL、Protocol、Model ID、API Key。
  await page.getByLabel("Connection base URL").fill(
    "https://api.deepseek.com/v1"
  );
  await expect(page.getByLabel("Connection protocol")).toHaveValue("openai_chat");
  await page.getByLabel("Connection model ID").fill("deepseek-reasoner");
  await page.getByLabel("Connection API key").fill("sk-test");
  // 元数据未探测/未手填前不可提交。
  await expect(page.getByRole("button", {name: "Save and start"})).toBeDisabled();
  await page.getByRole("button", {name: "Detect model"}).click();
  await expect(page.getByRole("button", {name: "Save and start"})).toBeEnabled();

  const accessibility = await new AxeBuilder({page})
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);

  await expect(page.getByLabel("Connection base URL")).toBeVisible();
  await expect(page.getByLabel("Connection protocol")).toHaveValue("openai_chat");
  await page.setViewportSize({width: 390, height: 844});
  await expect.poll(() => page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth
  )).toBeLessThanOrEqual(0);
  await expect(page.getByRole("button", {name: "Save and start"})).toBeVisible();
});

test("passes the WCAG A and AA accessibility scan", async ({page}) => {
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({colorScheme});
    await page.goto(baseURL);
    await expect(page.locator(".app")).toBeVisible();

    for (const state of ["empty", "settings"] as const) {
      if (state === "settings") {
        await page.getByRole("button", {name: "Settings"}).click();
      }
      const results = await new AxeBuilder({page})
        .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
        .analyze();
      expect(results.violations, `${colorScheme} ${state}`).toEqual([]);
    }
  }
});

test("groups Sessions by Workspace and reveals row actions on demand", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();

  const workspace = page.locator(".workspaceRow");
  await expect(workspace).toContainText(path.basename(workspaceDir));
  await expect(workspace).toHaveAttribute("aria-expanded", "true");
  const session = page.locator(".sessionRow[data-active]");
  await expect(session).toContainText("New Chat");

  await session.hover();
  await session.getByRole("button", {name: /Session actions for/}).click();
  await expect(session.getByRole("menuitem", {name: "Rename"})).toBeVisible();
  await expect(session.getByRole("menuitem", {name: "Pin"})).toBeVisible();
  await expect(session.getByRole("menuitem", {name: "Archive"})).toBeVisible();
  await expect(session.getByRole("menuitem", {name: "Delete"})).toBeVisible();

  await workspace.click();
  await expect(workspace).toHaveAttribute("aria-expanded", "false");
  await expect(page.locator(".sessionRow")).toHaveCount(0);
  await workspace.click();
  await expect(page.locator(".sessionRow")).toHaveCount(1);
});

// 不再自动关闭，搜索框计数断言过时。
test("shows and switches the Workspace Git branch", async ({page}) => {
  // Managed git execution 依赖受 Guard 管理的工具层；共享 fixture server 以
  // --enable-tools=false 启动，结构性不可用。本用例自起带工具的 server。
  const gitDataDir = await mkdtemp(path.join(tmpdir(), "qcode-web-e2e-git-"));
  const gitWorkspace = await mkdtemp(path.join(tmpdir(), "qcode-web-e2e-git-ws-"));
  await writeFile(path.join(gitWorkspace, "README.md"), "# Git fixture\n");
  execFileSync("git", ["init", "-q"], {cwd: gitWorkspace});
  execFileSync("git", ["add", "."], {cwd: gitWorkspace});
  execFileSync("git", [
    "-c", "core.hooksPath=/dev/null",
    "-c", "user.name=QCode",
    "-c", "user.email=fixture@qcode.invalid",
    "commit", "-qm", "branch fixture"
  ], {cwd: gitWorkspace});
  execFileSync("git", ["branch", "feature"], {cwd: gitWorkspace});
  const gitServer = spawn(binary, [
    "--workspace", gitWorkspace,
    "--data-dir", gitDataDir,
    "--provider-fixture", path.join(repositoryRoot, "testdata/providers/openai"),
    "--provider", "openai",
    "--model", "fixture-model",
    "--port", "0",
    "--no-open"
  ], {cwd: repositoryRoot, stdio: ["ignore", "pipe", "pipe"]});
  try {
  const gitBase = await runtimeURL(gitServer);
  await page.goto(gitBase);

  const panel = page.getByRole("complementary", {name: "Git tools"});
  const branch = execFileSync("git", ["branch", "--show-current"], {
    cwd: gitWorkspace, encoding: "utf8"
  }).trim();
  await expect(page.locator(".workspaceGroup select")).toHaveCount(0);
  await panel.getByRole("button", {name: branch, exact: true}).click();
  await panel.getByRole("button", {name: "feature", exact: true}).click();
  await expect(panel.getByRole("searchbox", {name: "Search Git branches"})).toHaveCount(0);
  await expect(panel.locator(".gitSummary").getByRole("button", {name: "feature", exact: true})).toBeVisible();
  expect(execFileSync(
    "git", ["branch", "--show-current"], {cwd: gitWorkspace, encoding: "utf8"}
  ).trim()).toBe("feature");
  } finally {
    if (gitServer.exitCode === null) {
      gitServer.kill("SIGINT");
      await Promise.race([
        new Promise<void>((resolve) => gitServer.once("exit", () => resolve())),
        new Promise<void>((resolve) => setTimeout(resolve, 10_000))
      ]);
      if (gitServer.exitCode === null) gitServer.kill("SIGKILL");
    }
    await rm(gitDataDir, {recursive: true, force: true});
    await rm(gitWorkspace, {recursive: true, force: true});
  }
});

test("adds a second Workspace and keeps its Sessions isolated", async ({page}) => {
  const secondary = await mkdtemp(
    path.join(tmpdir(), "qcode-web-workspace-secondary-")
  );
  try {
    await writeFile(
      path.join(secondary, "README.md"),
      "# Secondary workspace\n"
    );
    execFileSync("git", ["init", "-q"], {cwd: secondary});
    execFileSync("git", ["add", "README.md"], {cwd: secondary});
    execFileSync(binary, [
      "--workspace", secondary,
      "--data-dir", dataDir,
      "--provider-fixture", path.join(repositoryRoot, "testdata/providers/openai"),
      "--provider", "openai",
      "--model", "fixture-model",
      "--enable-tools=false",
      "--port", "0",
      "--no-open"
    ], {cwd: repositoryRoot});
    await page.goto(baseURL);

    const primaryGroup = page.locator(".workspaceGroup").filter({
      hasText: path.basename(workspaceDir)
    });
    const secondaryGroup = page.locator(".workspaceGroup").filter({
      hasText: path.basename(secondary)
    });
    await expect(page.locator(".workspaceGroup")).toHaveCount(2);
    await secondaryGroup.locator(".workspaceRow").click();
    await expect(secondaryGroup.locator(".workspaceHeader"))
      .toHaveAttribute("data-active", "true");
    await expect(page.locator("#git-tools .gitScope")).toContainText(path.basename(secondary));
    await secondaryGroup.locator(".workspaceCreateAction button").click();
    await expect(secondaryGroup.locator(".sessionRow")).toHaveCount(1);

    await primaryGroup.locator(".workspaceRow").click();
    await expect(primaryGroup.locator(".workspaceHeader"))
      .toHaveAttribute("data-active", "true");
    await expect(page.locator("#git-tools .gitScope")).toContainText(path.basename(workspaceDir));
    await primaryGroup.locator(".workspaceCreateAction button").click();
    await expect(primaryGroup.locator(".sessionRow")).toHaveCount(1);
    await expect(secondaryGroup.locator(".sessionRow")).toHaveCount(1);

    await expect(page.getByRole("button", {
      name: `Remove ${path.basename(workspaceDir)}`
    })).toHaveCount(1);
    await secondaryGroup.hover();
    await page.getByRole("button", {
      name: `Remove ${path.basename(secondary)}`
    }).click();
    await page.getByRole("alertdialog").getByRole("button", {name: "Remove workspace"}).click();
    await expect(page.locator(".workspaceGroup")).toHaveCount(1);

    await page.setViewportSize({width: 390, height: 844});
    await expect.poll(() => page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth
    )).toBeLessThanOrEqual(0);
  } finally {
    await rm(secondary, {recursive: true, force: true});
  }
});

// provider 请求，单流 fixture 第二次返回 409，回合被 Blocked。
test("creates a Session and completes a fixture-backed Turn", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();

  const composer = page.getByPlaceholder("Ask QCode");
  await expect(composer).toBeEnabled();
  await expect(page.getByLabel("Session details")).toHaveCount(0);
  await composer.fill("say hello");
  await page.getByRole("button", {name: "Send"}).click();

  await expect(page.getByText("hello", {exact: true}).last()).toBeVisible();
  const detailsToggle = page.getByRole("button", {name: /Execution details/});
  // 等待回合收束动画（展开态起步、下一帧收起）完成后再展开，避免把
  // 收起过程误切换回折叠。
  await expect(detailsToggle).toHaveAttribute("aria-expanded", "false");
  await detailsToggle.click();
  await expect(page.locator(".reasoningDisclosure")).toContainText(
    "I should answer briefly."
  );
  await expect(page.getByText("Working", {exact: true})).toHaveCount(0);
  // 标题由首回合并发生成（fixture 的专用标题流返回固定标题）。
  await expect(page.locator(".sessionRow[data-active]")).toContainText(
    "Browser fixture"
  );
});

test("inherits Approval when creating another Session", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();
  await page.getByLabel("Approval").selectOption("auto");
  await expect(page.getByLabel("Approval")).toHaveValue("auto");

  await page.getByRole("button", {name: /New session in /}).click();
  await expect(page.getByLabel("Approval")).toHaveValue("auto");
});

test("submits local attachments as verified Runtime context", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();

  const imagePath = path.join(workspaceDir, "pixel.png");
  await writeFile(imagePath, Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
    "base64"
  ));
  const picker = page.locator('input[type="file"][aria-label="Attach files"]');
  await expect(page.locator(
    '.composerControls button[aria-label="Attach files"]'
  )).toBeEnabled();
  await picker.setInputFiles([path.join(workspaceDir, "README.md"), imagePath]);
  await expect(page.getByLabel("Composer attachments")).toContainText(
    "Text"
  );
  await expect(page.getByLabel("Composer attachments")).toContainText(
    "picker"
  );
  await expect(page.getByLabel("Composer attachments")).toContainText("PNG");

  const operation = page.waitForRequest((request) =>
    request.url().endsWith("/api/v1/operation/submit") &&
    request.postDataJSON()?.kind === "turn.start"
  );
  await page.getByPlaceholder("Ask QCode").fill("review the attachment");
  await page.getByRole("button", {name: "Send"}).click();
  const payload = await operation;
  expect(payload.postDataJSON()).toMatchObject({
    payload: {
      prompt: "review the attachment",
      context: [{
        kind: "attachment",
        source: "native_picker",
        label: "README.md",
        media_type: "text/plain",
        explicit: true
      }, {
        kind: "image",
        source: "native_picker",
        label: "pixel.png",
        media_type: "image/png",
        explicit: true
      }]
    }
  });
  await expect(page.getByLabel("Composer attachments")).toHaveCount(0);
  await expect(page.getByRole("img", {name: "pixel.png"})).toBeVisible();
  await page.reload();
  await expect(page.getByRole("img", {name: "pixel.png"})).toBeVisible();
});

test("searches and invokes slash commands entirely from the keyboard", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();

  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill("/cont");
  const search = page.getByRole("searchbox", {name: "Search commands"});
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("cont");
  await expect(page.getByRole("menuitem", {name: /\/context/})).toBeVisible();
  await expect(page.getByRole("menuitem", {name: /\/compact/})).toHaveCount(0);
  const accessibility = await new AxeBuilder({page})
    .include(".commandMenu")
    .withTags(["wcag2a", "wcag2aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);
  await search.press("Enter");

  await expect(page.getByRole("dialog", {name: "Add context"})).toBeVisible();
  await page.getByRole("button", {name: "Close context browser"}).click();
  await page.getByRole("button", {name: "Commands"}).click();
  await expect(page.getByText("Recent", {exact: true})).toBeVisible();
  await expect(page.getByRole("menuitem").first()).toContainText("/context");
});

// 面板以 modal 打开拦截了空态按钮的点击。
test("keeps a long mobile draft scrollable above a resized visual viewport", async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();

  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill(Array.from({length: 120}, (_, index) => `line ${index}`).join("\n"));
  await composer.focus();
  await page.setViewportSize({width: 390, height: 420});

  const geometry = await composer.evaluate((element) => {
    const box = element.getBoundingClientRect();
    return {
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
      bottom: box.bottom,
      viewportHeight: window.visualViewport?.height ?? window.innerHeight,
      pageOverflow: document.documentElement.scrollWidth - window.innerWidth
    };
  });
  expect(geometry.clientHeight).toBeLessThanOrEqual(336);
  expect(geometry.scrollHeight).toBeGreaterThan(geometry.clientHeight);
  expect(geometry.bottom).toBeLessThanOrEqual(geometry.viewportHeight + 1);
  expect(geometry.pageOverflow).toBeLessThanOrEqual(0);
});

// 阻断，后续 Trajectory 断言无从满足。
test("opens the execution trajectory and inspects its event ledger", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: "Create session"}).click();
  await page.getByPlaceholder("Ask QCode").fill("say hello");
  await page.getByRole("button", {name: "Send"}).click();
  await expect(page.getByText("hello", {exact: true}).last()).toBeVisible();
  const detailsToggle = page.getByRole("button", {name: /Execution details/});
  // 等待回合收束动画（展开态起步、下一帧收起）完成后再展开，避免把
  // 收起过程误切换回折叠。
  await expect(detailsToggle).toHaveAttribute("aria-expanded", "false");
  await detailsToggle.click();
  await expect(page.locator(".reasoningDisclosure")).toContainText(
    "I should answer briefly."
  );

  await page.getByRole("button", {name: "Trajectory"}).click();
  const trajectory = page.getByLabel("Execution trajectory");
  await expect(trajectory).toBeVisible();
  await expect(trajectory.locator(".timelineLabels")).toHaveText("InputModelTools");
  await expect(trajectory.locator(".ledgerRow")).not.toHaveCount(0);

  await trajectory.locator(".ledgerRow").first().click();
  await expect(page.getByRole("complementary", {name: "Record inspector"}))
    .toBeVisible();
  await expect(page.getByRole("button", {name: "Previous record"})).toBeDisabled();
});

test("deletes the final Session after explicit confirmation", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();
  await expect(page.locator(".sessionRow")).toHaveCount(1);

  const session = page.locator(".sessionRow").first();
  await session.hover();
  await session.getByRole("button", {name: /Session actions for/}).click();
  await session.getByRole("menuitem", {name: "Delete"}).click();
  await page.getByRole("alertdialog")
    .getByRole("button", {name: "Delete session"}).click();

  await expect(page.locator(".sessionRow")).toHaveCount(0);
  await expect(page.getByRole("heading", {name: "Start a new session"})).toBeVisible();
  await expect(page.getByRole("button", {name: "Create session"})).toBeVisible();
  await expect(page.getByPlaceholder("Ask QCode")).toHaveCount(0);
  await expect(page.getByLabel("Session details")).toHaveCount(0);
});

// reload 断言之前即被 409 阻断。
test("restores the selected Session and transcript after a browser reload", async ({page}) => {
  await page.goto(baseURL);
  const sessionRows = page.locator(".sessionRow");
  const sessionCount = await sessionRows.count();
  await page.getByRole("button", {name: /New session in /}).click();
  await expect(sessionRows).toHaveCount(sessionCount + 1);

  const composer = page.getByPlaceholder("Ask QCode");
  await composer.fill("say hello");
  await page.getByRole("button", {name: "Send"}).click();
  await expect(page.getByText("hello", {exact: true}).last()).toBeVisible();
  const detailsToggle = page.getByRole("button", {name: /Execution details/});
  // 等待回合收束动画（展开态起步、下一帧收起）完成后再展开，避免把
  // 收起过程误切换回折叠。
  await expect(detailsToggle).toHaveAttribute("aria-expanded", "false");
  await detailsToggle.click();
  await expect(page.locator(".reasoningDisclosure")).toContainText(
    "I should answer briefly."
  );
  await expect(page.locator(".sessionRow[data-active]")).toContainText(
    "Browser fixture"
  );

  await page.reload();

  await expect(page.getByText("Connected", {exact: true})).toBeVisible();
  await expect(page.getByText("hello", {exact: true}).last()).toBeVisible();
  const detailsToggleAfterReload = page.getByRole("button", {name: /Execution details/});
  // 等待回合收束动画（展开态起步、下一帧收起）完成后再展开，避免把
  // 收起过程误切换回折叠。
  await expect(detailsToggle).toHaveAttribute("aria-expanded", "false");
  await detailsToggle.click();
  await expect(page.locator(".reasoningDisclosure")).toContainText(
    "I should answer briefly."
  );
  await expect(page.locator(".sessionRow[data-active]")).toContainText(
    "Browser fixture"
  );
  await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
});

// 文本（源码中无此字符串），断言指向已移除的 provenance 展示。
test("shows model routing and capabilities in Settings", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();
  await page.getByRole("button", {name: "Settings"}).click();
  await page.getByRole("button", {name: "Models"}).click();
  await expect(page.getByRole("button", {name: "Test connection"})).toBeVisible();

  const model = page.getByLabel("Settings model");
  // 复合值 provider\u0000model；同一 provider 下显示为纯模型 ID。
  await expect(model).toHaveValue(`fixture\u0000fixture-model`);
  // 向导流程：打开后填写端点与 Model ID，探测按钮就绪。
  await page.getByRole("button", {name: "Add model"}).click();
  const editor = page.getByRole("dialog", {name: "Add model"});
  await expect(editor).toBeVisible();
  await editor.getByLabel("Connection base URL").fill(
    "https://models.example.com/v1"
  );
  const editorModel = editor.getByLabel("Connection model ID");
  // hermetic fixture 不提供 /models 列表，探测/提交新模型的完整流程由
  // 单元测试（App.test.tsx）覆盖；此处验证向导校验与能力展示。
  await editorModel.fill("fixture-model-next");
  await expect(editor.getByRole("button", {name: "Detect model"})).toBeEnabled();
  await editor.getByRole("button", {name: "Cancel"}).click();
  await expect(editor).toHaveCount(0);
  await page.setViewportSize({width: 390, height: 844});
  await expect.poll(() => page.locator(".settingsDialog").evaluate((dialog) =>
    dialog.scrollWidth <= dialog.clientWidth
  )).toBe(true);
  await page.setViewportSize({width: 1280, height: 800});
  await expect(page.getByText("Context window")).toBeVisible();
  await expect(page.getByText("Prompt cache", {exact: true})).toBeVisible();
  await page.getByRole("button", {name: "Close settings"}).click();
  // 模型选择值编码为 "provider\u0000model"（跨 provider 选择）。
  await expect(page.getByLabel("Model"))
    .toHaveValue("fixture\u0000fixture-model");
});

test("persists and applies a workspace Agent preset", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();
  await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
  await page.getByRole("button", {name: "Settings"}).click();
  await page.getByRole("button", {name: "Agent preset"}).click();

  await page.getByLabel("Agent mode").selectOption("plan");
  await page.getByLabel("Maximum steps").fill("16");
  await page.getByLabel("Agent preset name").fill("Focused review");
  await page.getByLabel("Agent preset description").fill("Plan with bounded steps");
  await page.getByRole("button", {name: "Save new"}).click();
  const presetStatus = page.locator(".presetWorkbench").getByRole("status");
  await expect(presetStatus).toContainText("Preset created");
  const accessibility = await new AxeBuilder({page})
    .include(".settingsDialog")
    .withTags(["wcag2a", "wcag2aa"])
    .analyze();
  expect(accessibility.violations).toEqual([]);

  await page.getByRole("button", {name: "Discard"}).click();
  await page.getByRole("button", {name: "Apply to session"}).click();
  await expect(presetStatus).toContainText("Preset applied");
  await expect(page.getByLabel("Agent mode")).toHaveValue("plan");
  await expect(page.getByLabel("Maximum steps")).toHaveValue("16");

  await page.getByRole("button", {name: "Close settings"}).click();
  await page.reload();
  await page.getByRole("button", {name: "Settings"}).click();
  await page.getByRole("button", {name: "Agent preset"}).click();
  await expect(page.getByLabel("Saved agent preset")).toContainText("Focused review");

  await page.setViewportSize({width: 390, height: 844});
  await expect.poll(() => page.locator(".settingsDialog").evaluate((dialog) => {
    const box = dialog.getBoundingClientRect();
    const buttons = Array.from(dialog.querySelectorAll<HTMLElement>("button"))
      .filter((button) => button.offsetParent !== null)
      .map((button) => button.getBoundingClientRect());
    return dialog.scrollWidth <= dialog.clientWidth &&
      box.left >= 0 &&
      box.right <= window.innerWidth &&
      buttons.every(
        (button) => button.left >= box.left && button.right <= box.right
      );
  })).toBe(true);
});

test("browses workspace resources and restores an archived Session", async ({page}) => {
  await page.goto(baseURL);
  await page.getByRole("button", {name: /New session in /}).click();
  await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
  await openContextDetails(page);

  const fileEntry = page.locator(".contextResults button").filter({
    has: page.getByText("README.md", {exact: true})
  });
  await expect(fileEntry).toBeVisible();
  await fileEntry.click();
  await expect(page.locator(".contextPreview")).toBeVisible();
  const resourceContent = page.getByLabel("Workspace resource content");
  await resourceContent.focus();
  await resourceContent.evaluate((element: HTMLTextAreaElement) => {
    element.setSelectionRange(0, Math.min(5, element.value.length));
    element.dispatchEvent(new Event("select", {bubbles: true}));
    document.dispatchEvent(new Event("selectionchange", {bubbles: true}));
  });
  await page.getByRole("button", {name: "Add selection"}).click();
  await expect(page.getByLabel("Prompt context", {exact: true})).toContainText(
    /:1:1-1:6/
  );
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", {name: "Download resource"}).click()
  ]);
  expect(download.suggestedFilename()).toBe("README.md");

  const symbolSearch = page.getByLabel("Search workspace symbols");
  await symbolSearch.fill("helloFixture");
  await symbolSearch.press("Enter");
  const symbol = page.getByRole("button", {name: /helloFixture.*function.*main.go:3/});
  await expect(symbol).toBeVisible();
  await symbol.click();
  await expect(page.getByLabel("Prompt context", {exact: true})).toContainText("main.go");

  const imageEntry = page.locator(".contextResults button").filter({
    has: page.getByText("diagram.png", {exact: true})
  });
  await imageEntry.click();
  await expect(page.getByRole("img", {name: "diagram.png"})).toBeVisible();
  await page.getByRole("button", {name: "Add image"}).click();
  await expect(page.getByLabel("Prompt context", {exact: true})).toContainText(
    "diagram.png"
  );

  await page.getByRole("button", {name: "Close context browser"}).click();
  let activeSession = page.locator(".sessionRow[data-active]");
  await activeSession.hover();
  await activeSession.getByRole("button", {name: /Session actions for/}).click();
  await activeSession.getByRole("menuitem", {name: "Rename"}).click();
  const renameDialog = page.getByRole("dialog", {name: "Rename session"});
  await expect(renameDialog).toBeVisible();
  await renameDialog.getByLabel("Session title").fill("Archive Target");
  await renameDialog.getByRole("button", {name: "Rename"}).click();
  await expect(page.locator(".sessionRow").filter({
    hasText: "Archive Target"
  })).toBeVisible();

  activeSession = page.locator(".sessionRow[data-active]");
  await activeSession.hover();
  await activeSession.getByRole("button", {name: /Session actions for/}).click();
  await activeSession.getByRole("menuitem", {name: "Archive"}).click();
  await page.getByRole("alertdialog", {name: "Archive session?"})
    .getByRole("button", {name: "Archive"}).click();
  await expect(page.getByRole("heading", {name: "Archive Target", level: 1})).toHaveCount(0);

  await page.getByRole("button", {name: "Search sessions"}).click();
  await page.getByRole("button", {name: "Show archived"}).click();
  const archived = page.locator(".sessionRow").filter({
    has: page.getByText("Archive Target", {exact: true})
  });
  await archived.locator(".sessionSelect").click();
  await archived.hover();
  await archived.getByRole("button", {name: /Session actions for/}).click();
  await archived.getByRole("menuitem", {name: "Restore"}).click();
  await expect(page.getByPlaceholder("Ask QCode")).toBeEnabled();
});

// 面板默认自动展开为 modal，其头部按钮越过视口边界。
test("keeps primary UI inside supported viewports with reduced motion", async ({page}) => {
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
      await expect(page.locator(".app")).toBeVisible();

      const geometry = await page.evaluate(() => {
        const app = document.querySelector<HTMLElement>(".app");
        const outside: string[] = [];
        // 画布外元素（如 compact 模式下移出屏幕的会话抽屉）带 inert/aria-hidden，
        // 不属于“主界面按钮”，不计入视口内检查。
        const buttons = Array.from(document.querySelectorAll<HTMLElement>("button"))
          .filter((button) => button.offsetParent !== null &&
            !button.closest("[inert], [aria-hidden='true']"))
          .map((button) => {
            const box = button.getBoundingClientRect();
            if (box.left < 0 || box.right > window.innerWidth) {
              outside.push(`${button.getAttribute("aria-label") ?? button.textContent?.trim() ?? "?"} [${Math.round(box.left)},${Math.round(box.right)}] vw=${window.innerWidth}`);
            }
            return box;
          });
        return {
          appOverflow: app ? app.scrollWidth - app.clientWidth : -1,
          buttonsInside: buttons.every(
            (box) => box.left >= 0 && box.right <= window.innerWidth
          ),
          outside
        };
      });

      expect(geometry.appOverflow).toBeLessThanOrEqual(0);
      expect(geometry.outside, `viewport ${viewport.width}x${viewport.height} ${colorScheme}`).toEqual([]);
    }
  }

  await page.setViewportSize({width: 1024, height: 768});
  await page.emulateMedia({forcedColors: "active"});
  await page.goto(baseURL);
  await expect(page.getByRole("button", {name: /New session in /})).toBeVisible();
  await expect(page.getByRole("button", {name: "Settings"})).toBeVisible();

  await page.emulateMedia({forcedColors: "none"});
  await page.setViewportSize({width: 512, height: 384});
  const zoomed = await page.evaluate(() => ({
    overflow: document.documentElement.scrollWidth - window.innerWidth,
    composerVisible: Boolean(
      document.querySelector<HTMLElement>(".composer, .startupSetup")?.offsetParent
    )
  }));
  expect(zoomed.overflow).toBeLessThanOrEqual(0);
  expect(zoomed.composerVisible).toBe(true);

  await page.emulateMedia({forcedColors: "none", reducedMotion: "no-preference"});
  await page.evaluate(() => {
    const spinner = document.createElement("span");
    spinner.className = "spin";
    spinner.dataset.testid = "motion-probe";
    document.body.append(spinner);
  });
  await expect.poll(() => page.locator('[data-testid="motion-probe"]').evaluate(
    (element) => element.getAnimations()[0]?.effect?.getTiming().iterations
  )).toBe(Infinity);

  await page.emulateMedia({reducedMotion: "reduce"});
  await expect.poll(() => page.locator('[data-testid="motion-probe"]').evaluate(
    (element) => element.getAnimations()[0]?.effect?.getTiming().iterations ?? 0
  )).toBeLessThanOrEqual(1);
});

async function openContextDetails(page: Page): Promise<void> {
  await page.getByRole("button", {name: "Commands"}).click();
  await page.getByRole("menuitem", {name: /context/}).click();
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
  if (workspaces.length !== 1) throw new Error("Expected one ready workspace in the isolated browser fixture");
  const target = new URL(origin);
  target.searchParams.set("workspace", workspaces[0].id);
  return target.toString();
}
