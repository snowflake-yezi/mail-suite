import { expect, test, type Page } from "@playwright/test";

const activeSessionExpiry = new Date(
  Date.now() + 24 * 60 * 60 * 1000,
).toISOString();

const mailboxSession = {
  authenticated: true,
  principal: {
    id: "30000000-0000-4000-8000-000000000003",
    display_name: "Mailbox Fixture",
  },
  account_type: "mailbox",
  tenant_id: "30000000-0000-4000-8000-000000000001",
  mailbox: {
    id: "30000000-0000-4000-8000-000000000004",
    address: "mailbox@mail-suite.example.test",
  },
  permissions: [],
  expires_at: activeSessionExpiry,
  csrf_token: "csrf-token-with-at-least-32-bytes",
};

const administratorSession = {
  authenticated: true,
  principal: {
    id: "30000000-0000-4000-8000-000000000005",
    display_name: "Administrator Fixture",
  },
  account_type: "administrator",
  tenant_id: "30000000-0000-4000-8000-000000000001",
  permissions: ["portal.admin.access"],
  expires_at: activeSessionExpiry,
  csrf_token: "admin-csrf-token-with-at-least-32-bytes",
};

test.beforeEach(async ({ page }) => {
  await page.route("**/health/ready", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" }),
    });
  });
});

// useSession 让浏览器场景只模拟公开会话响应，不注入 token 或开发身份 header。
async function useSession(page: Page, session: unknown) {
  await page.route("**/api/v1/session", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(session),
    });
  });
}

// expectNoHorizontalOverflow 验证当前视口没有页面级横向溢出。
async function expectNoHorizontalOverflow(page: Page) {
  const hasHorizontalOverflow = await page.evaluate(
    () =>
      document.documentElement.scrollWidth >
      document.documentElement.clientWidth,
  );
  expect(hasHorizontalOverflow).toBe(false);
}

test("shows the OIDC login entry for an anonymous session", async ({
  page,
}) => {
  await useSession(page, { authenticated: false });
  await page.goto("/");

  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("img", { name: "Mail Suite" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "账号登录" })).toBeVisible();
  await expect(page.getByText("组织身份服务已连接")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "使用账号登录" }),
  ).toHaveAttribute("href", "/api/v1/auth/login");
  await expect(page.getByText("进入管理端", { exact: true })).toHaveCount(0);
  await expect(page.getByText("进入邮箱", { exact: true })).toHaveCount(0);
  await expectNoHorizontalOverflow(page);
});

test("does not reveal a mailbox while an anonymous route is guarded", async ({
  page,
}) => {
  await useSession(page, { authenticated: false });
  await page.goto("/mail/inbox");

  await expect(page).toHaveURL(/\/login\?return_to=%2Fmail%2Finbox$/);
  await expect(page.getByRole("heading", { name: "账号登录" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "收件箱" })).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "使用账号登录" }),
  ).toHaveAttribute("href", "/api/v1/auth/login?return_to=%2Fmail%2Finbox");
});

test("keeps the authenticated mailbox separate from management", async ({
  page,
}) => {
  await useSession(page, mailboxSession);
  await page.goto("/mail/inbox");

  await expect(page.getByRole("heading", { name: "收件箱" })).toBeVisible();
  await expect(page.getByLabel("当前账号：Mailbox Fixture")).toBeVisible();
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

  await page.goto("/admin/overview");
  await expect(
    page.getByRole("heading", { name: "当前账号无权访问此区域" }),
  ).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

test("keeps the authenticated administrator separate from mailbox", async ({
  page,
}) => {
  await useSession(page, administratorSession);
  await page.goto("/admin/overview");

  await expect(page.getByRole("heading", { name: "运行概览" })).toBeVisible();
  await expect(
    page.getByLabel("当前账号：Administrator Fixture"),
  ).toBeVisible();
  await expect(page.getByText("服务正常").first()).toBeVisible();
  await expect(page.getByRole("link", { name: "邮箱" })).toHaveCount(0);
  const adminMenu = page.getByRole("button", { name: "打开管理导航" });
  if (await adminMenu.isVisible()) {
    await adminMenu.click();
  }
  await expect(page.getByText("域名", { exact: true })).toBeVisible();

  await page.goto("/mail/inbox");
  await expect(
    page.getByRole("heading", { name: "当前账号无权访问此区域" }),
  ).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

test("clears the browser shell after a CSRF protected logout", async ({
  page,
}) => {
  await useSession(page, mailboxSession);
  await page.route("**/api/v1/auth/logout", async (route) => {
    expect(route.request().method()).toBe("POST");
    expect(route.request().headers()["x-csrf-token"]).toBe(
      mailboxSession.csrf_token,
    );
    await route.fulfill({ status: 204, body: "" });
  });
  await page.goto("/mail/inbox");

  await page.getByRole("button", { name: "退出登录" }).click();

  await expect(page).toHaveURL(/\/login\?return_to=%2Fmail%2Finbox$/);
  await expect(page.getByRole("heading", { name: "账号登录" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "收件箱" })).toHaveCount(0);
});

test("uses one persisted theme across authenticated product shells", async ({
  page,
}) => {
  await useSession(page, administratorSession);
  await page.goto("/admin/overview");
  await page.getByRole("combobox", { name: "主题" }).selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const storageKeys = await page.evaluate(() => Object.keys(localStorage));
  expect(storageKeys).toEqual(["mail-suite.theme"]);
  await expectNoHorizontalOverflow(page);
});
