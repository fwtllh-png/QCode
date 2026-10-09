import {defineConfig} from "@playwright/test";

export default defineConfig({
  testDir: "./tests/e2e",
  globalSetup: "./tests/e2e/sandbox-preflight.ts",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  snapshotPathTemplate: "{testDir}/{testFilePath}-snapshots/{arg}{ext}",
  expect: {
    timeout: 10_000,
    toHaveScreenshot: {
      animations: "disabled",
      caret: "hide",
      maxDiffPixelRatio: 0.01
    }
  },
  reporter: process.env.CI
    ? [["line"], ["html", {outputFolder: "../.tmp/playwright-report", open: "never"}]]
    : "line",
  outputDir: "../.tmp/playwright-results",
  use: {
    browserName: "chromium",
    contextOptions: {
      reducedMotion: "reduce"
    },
    trace: "retain-on-failure",
    screenshot: "only-on-failure"
  }
});
