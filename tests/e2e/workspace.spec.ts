import { expect, test } from "@playwright/test";

test.beforeEach(async ({ page }) => {
  await page.route("**/health/ready", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" }),
    });
  });
});

test("renders a usable service workspace without horizontal overflow", async ({
  page,
}) => {
  await page.goto("/");

  await expect(page.getByRole("heading", { name: "运行概览" })).toBeVisible();
  await expect(page.getByRole("status")).toHaveText("服务正常");
  const refreshButton = page.getByRole("button", { name: "刷新服务状态" });
  await expect(refreshButton).toBeEnabled();

  await page.keyboard.press("Tab");
  if (
    !(await refreshButton.evaluate(
      (element) => element === document.activeElement,
    ))
  ) {
    await page.keyboard.press("Tab");
  }
  await expect(refreshButton).toBeFocused();

  const hasHorizontalOverflow = await page.evaluate(
    () =>
      document.documentElement.scrollWidth >
      document.documentElement.clientWidth,
  );
  expect(hasHorizontalOverflow).toBe(false);
});
