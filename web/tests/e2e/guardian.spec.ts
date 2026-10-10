import {expect, test} from "@playwright/test";
import {createServer, type ViteDevServer} from "vite";
import path from "node:path";
import {fileURLToPath} from "node:url";

let server: ViteDevServer;
let url: string;
test.beforeAll(async () => {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
  server = await createServer({root, configFile: path.join(root, "vite.config.ts"),
    server: {host: "127.0.0.1", port: 0, open: false, hmr: false, watch: null}});
  await server.listen();
  const address = server.httpServer!.address();
  if (!address || typeof address === "string") throw new Error("missing fixture port");
  url = `http://127.0.0.1:${address.port}/tests/e2e/fixtures/guardian.html`;
});
test.afterAll(async () => { await server?.close(); });

test("successful review remains collapsed across reload without implying execution", async ({page}) => {
  await page.goto(`${url}?mode=allow`);
  const card = page.locator('.toolDisclosure[data-call-id="call"]');
  await expect(card).toHaveCount(1);
  await expect(page.getByLabel("Automatic review details")).toHaveCount(0);
  await card.locator(".disclosureRow").click();
  await expect(page.getByText("Authorization: automatic approval", {exact: true})).toBeVisible();
  await expect(page.getByText("Execution: no execution receipt", {exact: true})).toBeVisible();
  await page.reload();
  await expect(card).toHaveCount(1);
  await expect(page.getByLabel("Automatic review details")).toHaveCount(0);
  await expect(page.locator(".approvalComposer")).toHaveCount(0);
});

for (const decision of ["approve", "deny", "cancel"]) {
  test(`replayed Guardian failure can ${decision} through the existing approval card`, async ({page}) => {
    await page.goto(`${url}?mode=${decision}`);
    await expect(page.getByRole("note")).toHaveText("Automatic review could not complete. Your confirmation is required.");
    await page.reload();
    const approval = page.locator(".approvalComposer");
    await expect(approval).toHaveCount(1);
    await expect(approval.getByText("./build.sh", {exact: true})).toBeVisible();
    await approval.getByRole("button", {name: decision === "approve" ? "Approve once" : decision === "deny" ? "Deny" : "Stop turn"}).click();
    await expect(approval).toHaveCount(0);
    await page.reload();
    await expect(approval).toHaveCount(0);
    const details = page.getByRole("button", {name: /Execution details/});
    if (await details.count()) await details.click();
    await expect(page.locator('.toolDisclosure[data-call-id="call"]')).toHaveCount(1);
  });
}
