import architectureDarkUrl from '../../../../static/architecture-dark.svg?url'
import architectureUrl from '../../../../static/architecture.svg?url'
import architectureZhCnDarkUrl from '../../../../static/architecture.zh-CN-dark.svg?url'
import architectureZhCnUrl from '../../../../static/architecture.zh-CN.svg?url'
import { useSiteContext } from './SiteContext'

interface ArchitectureDiagramProps {
  className?: string
  eager?: boolean
}

const architectureUrls = {
  en: {
    light: architectureUrl,
    dark: architectureDarkUrl,
  },
  'zh-CN': {
    light: architectureZhCnUrl,
    dark: architectureZhCnDarkUrl,
  },
} as const

export function ArchitectureDiagram({ className = '', eager = false }: ArchitectureDiagramProps) {
  const { isDark, locale } = useSiteContext()
  const src = architectureUrls[locale][isDark ? 'dark' : 'light']
  const alt = locale === 'zh-CN' ? 'OnlyBoxes 架构图' : 'OnlyBoxes architecture'

  return (
    <img
      src={src}
      alt={alt}
      width={1200}
      height={480}
      loading={eager ? 'eager' : 'lazy'}
      fetchPriority={eager ? 'high' : 'auto'}
      className={`block h-auto w-full ${className}`.trim()}
    />
  )
}
