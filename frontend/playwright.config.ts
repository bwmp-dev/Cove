import { defineConfig } from "@playwright/test";

if (!process.env.COVE_BROWSER_URL || !process.env.COVE_BROWSER_PASSWORD) {
  throw new Error("Run scripts/test-browser.py to provision an isolated panel.");
}

export default defineConfig({
  testDir: "./e2e",
  workers: 1,
  timeout: 30_000,
  use: {
    baseURL: process.env.COVE_BROWSER_URL,
    ignoreHTTPSErrors: true,
    trace: "off",
    screenshot: "off",
    video: "off",
  },
});
