import { test, expect } from "@playwright/test";
test("browser SQLite conformance and transaction ownership", async ({ page }) => {
  await page.goto("/tests/browser/");
  await page.waitForFunction(() => globalThis.testResult !== null);
  const result = await page.evaluate(() => globalThis.testResult);
  expect(result.error).toBeNull();
  expect(result.passed).toBeGreaterThanOrEqual(23);
});
