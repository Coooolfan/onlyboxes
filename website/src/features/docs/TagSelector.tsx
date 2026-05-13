import { useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'

interface GitHubTag {
  name: string
}

interface TagSelectorProps {
  locale?: 'en' | 'zh-CN'
  repo?: string
  limit?: number
  defaultTag?: string
}

const copy = {
  en: {
    label: 'Version',
    loading: 'loading…',
    failed: 'failed to load',
    copy: 'Copy',
    copied: 'Copied',
    releases: 'Browse all releases',
  },
  'zh-CN': {
    label: '版本',
    loading: '加载中…',
    failed: '加载失败',
    copy: '复制',
    copied: '已复制',
    releases: '查看全部发行版',
  },
} as const

function buildCommand(tag: string) {
  return `curl -fsSL https://onlybox.es/install.sh | bash -s -- --tag ${tag}`
}

export function TagSelector({
  locale = 'en',
  repo = 'Coooolfan/onlyboxes',
  limit = 20,
  defaultTag = '0.5.0',
}: TagSelectorProps) {
  const t = copy[locale]
  const [tags, setTags] = useState<string[] | null>(null)
  const [loadFailed, setLoadFailed] = useState(false)
  const [selected, setSelected] = useState(defaultTag)
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    let cancelled = false
    fetch(`https://api.github.com/repos/${repo}/tags?per_page=${limit}`, {
      headers: { Accept: 'application/vnd.github+json' },
    })
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json() as Promise<GitHubTag[]>
      })
      .then((data) => {
        if (cancelled) return
        const names = data.map((tag) => tag.name)
        setTags(names)
        if (names.length > 0 && !names.includes(defaultTag)) {
          setSelected(names[0])
        }
      })
      .catch(() => {
        if (!cancelled) setLoadFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [repo, limit, defaultTag])

  const handleCopy = () => {
    navigator.clipboard.writeText(buildCommand(selected))
    setCopied(true)
    window.setTimeout(() => setCopied(false), 2000)
  }

  const options = tags ?? [defaultTag]
  const releasesUrl = `https://github.com/${repo}/releases`

  return (
    <div className="mt-6">
      <div className="mb-2 flex items-center gap-2 text-xs text-(--ob-muted)">
        <label htmlFor="tag-selector-version" className="font-medium">
          {t.label}
        </label>
        <select
          id="tag-selector-version"
          value={selected}
          onChange={(event) => setSelected(event.target.value)}
          className="rounded border border-(--ob-line) bg-(--ob-bg) px-2 py-1 font-mono text-xs text-(--ob-ink) focus:outline-none focus:ring-1 focus:ring-(--ob-ink)"
        >
          {options.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </select>
        <span className="text-(--ob-muted)">
          {tags === null && !loadFailed ? t.loading : null}
          {loadFailed ? (
            <a
              href={releasesUrl}
              target="_blank"
              rel="noreferrer"
              className="underline decoration-(--ob-line) underline-offset-4 hover:decoration-(--ob-ink)"
            >
              {t.failed} — {t.releases}
            </a>
          ) : null}
        </span>
      </div>
      <div className="group relative">
        <pre className="overflow-x-auto rounded border border-(--ob-line) bg-(--ob-pre-bg) px-5 py-4 text-sm leading-7 text-(--ob-pre-text)">
          <code>{buildCommand(selected)}</code>
        </pre>
        <button
          type="button"
          onClick={handleCopy}
          aria-label={t.copy}
          className="absolute top-2.5 right-2.5 rounded bg-(--ob-pre-bg) p-1.5 text-(--ob-pre-text) opacity-0 transition-opacity group-hover:opacity-70"
        >
          {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
        </button>
      </div>
    </div>
  )
}
