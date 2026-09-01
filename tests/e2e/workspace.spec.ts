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

// expectNoHorizontalOverflow 验证当前视口没有页面级横向溢出。
async function expectNoHorizontalOverflow(
  page: import("@playwright/test").Page,
) {
  const hasHorizontalOverflow = await page.evaluate(
    () =>
      document.documentElement.scrollWidth >
      document.documentElement.clientWidth,
  );
  expect(hasHorizontalOverflow).toBe(false);
}

test("shows the branded login shell without choosing an account type", async ({
  page,
}) => {
  await page.goto("/");

  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("img", { name: "Mail Suite" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "账号登录" })).toBeVisible();
  await expect(page.getByText("认证服务尚未接入")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "使用账号登录" }),
  ).toBeDisabled();
  await expect(page.getByText("进入管理端", { exact: true })).toHaveCount(0);
  await expect(page.getByText("进入邮箱", { exact: true })).toHaveCount(0);

  await expectNoHorizontalOverflow(page);
});

test("keeps the mailbox preview separate from management navigation", async ({
  page,
}) => {
  await page.goto("/mail/inbox");

  await expect(page.getByRole("heading", { name: "收件箱" })).toBeVisible();
  await expect(page.getByRole("status").last()).toContainText("邮箱尚未连接");
  await expect(page.getByRole("button", { name: "写邮件" })).toBeDisabled();
  await expect(page.getByRole("link", { name: /管理/ })).toHaveCount(0);

  const mailboxMenu = page.getByRole("button", { name: "打开邮箱导航" });
  if (await mailboxMenu.isVisible()) {
    await mailboxMenu.click();
  }
  await page.getByRole("link", { name: "星标" }).click();
  await expect(page).toHaveURL(/\/mail\/starred$/);
  await expect(
    page.getByRole("heading", { name: "星标邮件", exact: true }),
  ).toBeVisible();

  await expectNoHorizontalOverflow(page);
});

test("keeps the admin preview separate from mailbox navigation", async ({
  page,
}) => {
  await page.goto("/admin/overview");

  await expect(page.getByRole("heading", { name: "运行概览" })).toBeVisible();
  await expect(page.getByText("服务正常").first()).toBeVisible();
  await expect(
    page.getByRole("button", { name: "刷新服务状态" }),
  ).toBeEnabled();
  await expect(page.getByRole("link", { name: "邮箱" })).toHaveCount(0);
  const adminMenu = page.getByRole("button", { name: "打开管理导航" });
  if (await adminMenu.isVisible()) {
    await adminMenu.click();
  }
  await expect(page.getByText("域名", { exact: true })).toBeVisible();

  await expectNoHorizontalOverflow(page);
});

test("uses one persisted theme across the login and preview shells", async ({
  page,
}) => {
  await page.goto("/login");
  await page.getByRole("combobox", { name: "主题" }).selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");

  await page.goto("/mail/inbox");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectNoHorizontalOverflow(page);
});
