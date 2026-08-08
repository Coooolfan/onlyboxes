import { useState } from 'react'
import dockerDarkUrl from '../../../../static/public-preview-seq-docker-dark.svg?url'
import dockerUrl from '../../../../static/public-preview-seq-docker.svg?url'
import dockerZhCnDarkUrl from '../../../../static/public-preview-seq-docker.zh-CN-dark.svg?url'
import dockerZhCnUrl from '../../../../static/public-preview-seq-docker.zh-CN.svg?url'
import e2bDarkUrl from '../../../../static/public-preview-seq-e2b-dark.svg?url'
import e2bUrl from '../../../../static/public-preview-seq-e2b.svg?url'
import e2bZhCnDarkUrl from '../../../../static/public-preview-seq-e2b.zh-CN-dark.svg?url'
import e2bZhCnUrl from '../../../../static/public-preview-seq-e2b.zh-CN.svg?url'
import { useSiteContext } from '../site/SiteContext'

type Flow = 'docker' | 'e2b'

const tabCopy = {
  en: {
    docker: 'Docker / Boxlite',
    e2b: 'E2B',
  },
  'zh-CN': {
    docker: 'Docker / Boxlite',
    e2b: 'E2B',
  },
} as const

const flowUrls = {
  en: {
    docker: { light: dockerUrl, dark: dockerDarkUrl },
    e2b: { light: e2bUrl, dark: e2bDarkUrl },
  },
  'zh-CN': {
    docker: { light: dockerZhCnUrl, dark: dockerZhCnDarkUrl },
    e2b: { light: e2bZhCnUrl, dark: e2bZhCnDarkUrl },
  },
} as const

export function PublicPreviewSequenceDiagram() {
  const { isDark, locale } = useSiteContext()
  const [flow, setFlow] = useState<Flow>('docker')
  const t = tabCopy[locale]
  const src = flowUrls[locale][flow][isDark ? 'dark' : 'light']
  const alt = locale === 'zh-CN' ? '公开预览子域名代理请求时序图' : 'Public preview subdomain proxy request sequence'

  const tabClass = (active: boolean) =>
    `rounded px-3 py-1.5 text-sm font-medium transition-colors duration-300 ${
      active
        ? isDark
          ? 'bg-neutral-800 text-white'
          : 'bg-neutral-100 text-neutral-950'
        : isDark
          ? 'text-neutral-400 hover:text-white'
          : 'text-neutral-500 hover:text-neutral-950'
    }`

  return (
    <div className="mt-6">
      <div className="mb-3 flex gap-1">
        <button type="button" className={tabClass(flow === 'docker')} onClick={() => setFlow('docker')}>
          {t.docker}
        </button>
        <button type="button" className={tabClass(flow === 'e2b')} onClick={() => setFlow('e2b')}>
          {t.e2b}
        </button>
      </div>
      <img
        src={src}
        alt={alt}
        width={1100}
        height={580}
        loading="lazy"
        className="block h-auto w-full"
      />
    </div>
  )
}
