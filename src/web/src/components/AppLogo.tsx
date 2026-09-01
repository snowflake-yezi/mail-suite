import { Link } from 'react-router-dom'

import logoUrl from '../../../../logo.jpg'

// AppLogoProps 描述不同产品外壳复用品牌标识时的尺寸和文案。
interface AppLogoProps {
  subtitle: string
  to?: string
  size?: 'large' | 'compact'
}

// AppLogo 使用仓库品牌图源并保持产品名称、替代文本和裁切规则一致。
export function AppLogo({ subtitle, to, size = 'compact' }: AppLogoProps) {
  const content = (
    <>
      <img className="app-logo__image" src={logoUrl} alt="Mail Suite" />
      <span className="app-logo__copy">
        <strong>Mail Suite</strong>
        <span>{subtitle}</span>
      </span>
    </>
  )
  const className = `app-logo app-logo--${size}`

  return to ? (
    <Link className={className} to={to}>
      {content}
    </Link>
  ) : (
    <div className={className}>{content}</div>
  )
}
