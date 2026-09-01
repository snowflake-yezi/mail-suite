// JPG 模块声明允许 Vite 将仓库品牌图源作为构建期 URL 导入。
declare module '*.jpg' {
  const url: string
  export default url
}
