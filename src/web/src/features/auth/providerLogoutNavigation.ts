// navigateToProviderLogout 使用顶层替换导航进入已由后端验证的 IdP 退出地址。
export function navigateToProviderLogout(url: string): void {
  window.location.replace(url)
}
