import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: "test", testMatch: "browser.spec.mjs", timeout: 120000,
  webServer: { command: "python3 -m http.server 8914 --bind 127.0.0.1 --directory ../..", url: "http://127.0.0.1:8914/tests/browser/", reuseExistingServer: !process.env.CI },
  use: { baseURL: "http://127.0.0.1:8914" },
});
