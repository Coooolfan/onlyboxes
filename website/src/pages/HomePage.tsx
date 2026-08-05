import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  BookOpen,
  Check,
  Copy,
  Github,
  Languages,
  Moon,
  Sun,
} from 'lucide-react'
import { useSiteContext } from '../features/site/SiteContext'
import { ArchitectureDiagram } from '../features/site/ArchitectureDiagram'
import { docsLocaleLabels, getDocsRootHref, getOtherDocsLocale } from '../features/docs/registry'

const INSTALL_CMD = 'curl -fsSL https://onlybox.es/install.sh | bash'

const homeCopy = {
  en: {
    rotatingTexts: [
      'Run untrusted code safely?',
      'Self-host your code sandbox?',
      'Power your AI agents with skills?',
    ],
    subtitle: 'A self-hosted code execution sandbox platform for individuals and small teams.',
    documentation: 'Documentation',
    github: 'GitHub',
  },
  'zh-CN': {
    rotatingTexts: [
      '执行不受信任的代码？',
      '自托管代码沙箱？',
      '为 AI 智能体提供 Skills 运行环境？',
    ],
    subtitle: '面向个人与小型团队的自托管代码执行沙箱平台。',
    documentation: '文档',
    github: 'GitHub',
  },
} as const

const RotatingText = ({ texts }: { texts: readonly string[] }) => {
  const [index, setIndex] = useState(0)
  const [fade, setFade] = useState(true)

  useEffect(() => {
    const interval = window.setInterval(() => {
      setFade(false)
      window.setTimeout(() => {
        setIndex((current) => (current + 1) % texts.length)
        setFade(true)
      }, 500)
    }, 3000)

    return () => window.clearInterval(interval)
  }, [texts.length])

  return (
    <div className="mb-4 flex h-6 items-center">
      <span
        className={`text-sm text-neutral-500 transition-opacity duration-500 ease-in-out ${
          fade ? 'opacity-100' : 'opacity-0'
        }`}
      >
        {texts[index]}
      </span>
    </div>
  )
}

const InstallCommand = ({ isDark }: { isDark: boolean }) => {
  const [copied, setCopied] = useState(false)

  const handleCopy = async () => {
    await navigator.clipboard.writeText(INSTALL_CMD)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div
      className={`mt-6 flex w-fit max-w-full items-center rounded-lg border px-3 py-2.5 font-mono text-sm ${
        isDark ? 'border-neutral-800 bg-neutral-900/60 text-neutral-300' : 'border-neutral-200 bg-neutral-50 text-neutral-600'
      } transition-colors duration-300`}
    >
      <span className={`mr-1.5 select-none ${isDark ? 'text-neutral-600' : 'text-neutral-400'}`}>$</span>
      <code className="min-w-0 overflow-x-auto whitespace-nowrap">{INSTALL_CMD}</code>
      <button
        onClick={handleCopy}
        className={`ml-2 shrink-0 rounded p-1 ${
          isDark ? 'text-neutral-500 hover:bg-neutral-800 hover:text-white' : 'text-neutral-400 hover:bg-neutral-200 hover:text-black'
        } transition-colors`}
        aria-label="Copy command"
      >
        {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
  )
}

function HomePage() {
  const { isDark, setIsDark, locale, setLocale } = useSiteContext()
  const copy = homeCopy[locale]
  const targetLocale = getOtherDocsLocale(locale)

  return (
    <div
      className={`relative flex min-h-screen flex-col overflow-hidden font-sans ${
        isDark ? 'bg-black text-white selection:bg-neutral-800' : 'bg-white text-black selection:bg-neutral-200'
      } transition-colors duration-300`}
    >
      <header
        className={`flex items-center justify-between border-b px-6 py-4 ${
          isDark ? 'border-neutral-900' : 'border-neutral-100'
        } transition-colors duration-300`}
      >
        <div className="flex items-center gap-2">
          <img src="/favicon.png" alt="OnlyBoxes" className="h-5 w-5 rounded" />
          <span className="text-sm font-semibold tracking-tight">OnlyBoxes</span>
        </div>
        <div className="flex items-center gap-4">
          <button
            onClick={() => setIsDark((value) => !value)}
            className={`rounded-md p-2 ${
              isDark ? 'text-neutral-400 hover:bg-neutral-900 hover:text-white' : 'text-neutral-500 hover:bg-neutral-100 hover:text-black'
            } transition-colors duration-300`}
            aria-label="Toggle theme"
          >
            {isDark ? <Sun className="h-5 w-5" /> : <Moon className="h-5 w-5" />}
          </button>
          <button
            onClick={() => setLocale(targetLocale)}
            className={`flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium ${
              isDark ? 'text-neutral-400 hover:bg-neutral-900 hover:text-white' : 'text-neutral-500 hover:bg-neutral-100 hover:text-black'
            } transition-colors duration-300`}
            aria-label={docsLocaleLabels[targetLocale]}
          >
            <Languages className="h-4 w-4" />
            <span className="hidden sm:inline">{docsLocaleLabels[targetLocale]}</span>
          </button>
        </div>
      </header>

      <main className="flex min-h-[calc(100vh-65px)] flex-1 flex-col justify-center px-6 py-12 lg:px-12 xl:px-20">
        <div className="flex flex-col items-center gap-12 xl:flex-row xl:gap-20">
          <div className="z-10 flex w-full shrink-0 flex-col justify-center xl:w-[450px]">
            <RotatingText texts={copy.rotatingTexts} />

            <h1
              className={`mb-6 text-6xl leading-none font-bold tracking-tighter md:text-7xl lg:text-8xl ${
                isDark ? 'text-white' : 'text-black'
              } transition-colors duration-300`}
            >
              OnlyBoxes
            </h1>

            <p
              className={`mb-10 text-lg leading-relaxed font-light ${
                isDark ? 'text-neutral-400' : 'text-neutral-500'
              } transition-colors duration-300`}
            >
              {copy.subtitle}
            </p>

            <div className="flex flex-col items-start gap-4 sm:flex-row sm:items-center">
              <Link
                to={getDocsRootHref(locale)}
                className={`flex w-full items-center justify-center gap-2 rounded-md px-6 py-3 font-medium shadow-md transition-colors sm:w-auto ${
                  isDark ? 'bg-white text-black hover:bg-neutral-200' : 'bg-black text-white hover:bg-neutral-800'
                }`}
              >
                <BookOpen className="h-4 w-4" />
                {copy.documentation}
              </Link>
              <a
                href="https://github.com/Coooolfan/onlyboxes"
                target="_blank"
                rel="noopener noreferrer"
                className={`flex w-full items-center justify-center gap-2 rounded-md border px-6 py-3 font-medium transition-all sm:w-auto ${
                  isDark ? 'border-neutral-700 text-white hover:bg-neutral-900' : 'border-neutral-200 text-black hover:border-neutral-300 hover:bg-neutral-50'
                }`}
              >
                <Github className="h-4 w-4" />
                {copy.github}
              </a>
            </div>
          </div>

          <div className="relative flex w-full min-w-0 flex-1 items-center justify-center xl:w-auto">
            <div
              className={`absolute inset-0 -z-10 rounded-full bg-neutral-900/20 blur-3xl transition-opacity duration-500 ${
                isDark ? 'opacity-100' : 'opacity-0'
              }`}
            />
            <div className="relative w-full max-w-[1000px]">
              <ArchitectureDiagram eager />
            </div>
          </div>
        </div>

        <InstallCommand isDark={isDark} />
      </main>
    </div>
  )
}

export default HomePage
