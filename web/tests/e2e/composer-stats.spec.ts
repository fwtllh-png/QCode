import {expect, test} from "@playwright/test";
import {createServer, type ViteDevServer} from "vite";
import path from "node:path";
import {fileURLToPath} from "node:url";

let server: ViteDevServer;
let url: string;

test.beforeAll(async () => {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
  server = await createServer({
    root, configFile: path.join(root, "vite.config.ts"),
    server: {host: "127.0.0.1", port: 0, open: false, hmr: false, watch: null}
  });
  await server.listen();
  const address = server.httpServer!.address();
  if (!address || typeof address === "string") throw new Error("missing fixture port");
  url = `http://127.0.0.1:${address.port}/tests/e2e/fixtures/streaming.html?scrolling=draft`;
});
test.afterAll(async () => { await server?.close(); });

for (const width of [1440, 390, 320]) {
  test(`statistics remain readable and keyboard accessible at ${width}px`, async ({page}, testInfo) => {
    await page.setViewportSize({width, height: 844});
    await page.emulateMedia({colorScheme: width === 390 ? "dark" : "light"});
    await page.goto(url);
    await expect(page.locator(".assistantMarkdown")).toHaveCount(11);
    await page.evaluate(() => document.dispatchEvent(new CustomEvent("fixture:events", {detail: [
      {kind: "usage", data: {context: {estimated_tokens: 32768, stable_tokens: 8192,
        history_user_tokens: 8192, history_assistant_tokens: 8192, tool_definition_tokens: 8192}}},
      {kind: "turn.receipt", data: {
        context_budget: {active_tokens: 32768, max_context_tokens: 131072},
        input_tokens: 24600, output_tokens: 2855, reasoning_tokens: 2434,
        tool_execution: {business: 2, failed: 1}, cost_known: false,
        routes: [{purpose: "act", provider: "provider", model: "deepseek-flash"}],
        latency: {total_ms: 15107, first_token_ms: 1495, provider_ms: 14551,
          tool_ms: 0, approval_wait_ms: 0, verify_ms: 0}
      }},
      {kind: "turn.completed", data: {text: "Completed fixture"}}
    ]})));
    const trigger = page.getByRole("button", {name: /of context used\. Run statistics/});
    await expect(page.locator(".composerControls .contextMeter")).toHaveCount(1);
    await expect(trigger).toHaveText("");
    await expect(trigger).toBeInViewport();
    const panel = page.getByRole("dialog", {name: "Run statistics details"});
    const externalFocus = page.getByRole("button", {name: "Run streaming fixture"});
    await externalFocus.focus();
    await trigger.hover();
    await expect(panel).toBeVisible();
    await expect(panel.getByRole("region", {name: "Context usage"})).toBeVisible();
    await expect(externalFocus).toBeFocused();
    const triggerBox = (await trigger.boundingBox())!;
    const hoverBox = (await panel.boundingBox())!;
    // Cross the physical gap instead of teleporting directly into the card.
    await page.mouse.move(triggerBox.x + 10, hoverBox.y + hoverBox.height + 4);
    await expect(panel).toBeVisible();
    await page.mouse.move(hoverBox.x + 10, hoverBox.y + 10, {steps: 8});
    await expect(panel).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(externalFocus).toBeFocused();
    await trigger.hover();
    await expect(panel).toBeVisible();
    await page.mouse.move(0, 0);
    await expect(panel).toBeHidden();
    await trigger.focus();
    await page.keyboard.press("Enter");
    await expect(panel).toBeVisible();
    await expect(panel).toBeFocused();
    await expect(panel.getByText("Completed", {exact: true})).toBeVisible();
    await expect(panel.getByText("27,455")).toBeVisible();
    const box = await panel.boundingBox();
    expect(box?.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(width);
    expect(box?.y).toBeGreaterThanOrEqual(0);
    expect(await panel.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true);
    await page.screenshot({path: testInfo.outputPath("statistics.png")});
    await panel.getByRole("region", {name: "Session totals"}).scrollIntoViewIfNeeded();
    await expect(panel.getByText("Turns started")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(trigger).toBeFocused();
    await trigger.click();
    await page.mouse.move(0, 0);
    await expect(panel).toBeVisible();
    await page.getByRole("button", {name: "Close statistics"}).click();
    await expect(panel).toBeHidden();
  });
}
