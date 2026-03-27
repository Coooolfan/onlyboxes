import { type ElementType, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  BookOpen,
  Box,
  Check,
  Copy,
  Cpu,
  Github,
  Moon,
  Package,
  Blocks,
  Server,
  Sun,
  Terminal,
  User,
} from 'lucide-react'

const INSTALL_CMD = 'curl -fsSL https://onlybox.es/install.sh | bash -s -- --tag 0.1.5'

const RotatingText = () => {
  const texts = [
    'Run untrusted code safely?',
    'Self-host your code sandbox?',
    'Power your AI agents with skills?',
  ]
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

const FlowLine = ({ d, isDark }: { d: string; isDark: boolean }) => (
  <g>
    <path
      d={d}
      stroke={isDark ? '#222' : '#E5E5E5'}
      strokeWidth="1.5"
      fill="none"
      className="transition-colors duration-300"
    />
    <path
      d={d}
      stroke={isDark ? '#FFF' : '#000'}
      strokeWidth="1.5"
      strokeDasharray="4 8"
      fill="none"
      className="transition-colors duration-300"
    >
      <animate attributeName="stroke-dashoffset" from="12" to="0" dur="1s" repeatCount="indefinite" />
    </path>
  </g>
)

interface SvgNodeProps {
  x: number
  y: number
  width?: number
  height?: number
  title: string
  subtitle?: string
  icon?: ElementType
  dashed?: boolean
  active?: boolean
  isDark: boolean
}

const SvgNode = ({
  x,
  y,
  width = 220,
  height = 56,
  title,
  subtitle,
  icon: Icon,
  dashed = false,
  active = false,
  isDark,
}: SvgNodeProps) => (
  <g
    transform={`translate(${x}, ${y})`}
    filter={isDark ? 'url(#shadow-dark)' : 'url(#shadow-sm)'}
    className="transition-all duration-300"
  >
    <rect
      width={width}
      height={height}
      rx={8}
      fill={isDark ? '#141414' : '#FFFFFF'}
      stroke={active ? (isDark ? '#555' : '#A3A3A3') : isDark ? '#2A2A2A' : '#E5E5E5'}
      strokeWidth={1}
      strokeDasharray={dashed ? '4 4' : 'none'}
      className="transition-colors duration-300"
    />
    <rect
      x={12}
      y={12}
      width={32}
      height={32}
      rx={6}
      fill={isDark ? '#1A1A1A' : '#F5F5F5'}
      stroke={isDark ? '#2A2A2A' : '#E5E5E5'}
      strokeWidth={1}
      className="transition-colors duration-300"
    />
    {Icon ? (
      <svg x={16} y={16} width={24} height={24}>
        <Icon
          size={24}
          className={`${isDark ? 'text-white' : 'text-black'} transition-colors duration-300`}
          strokeWidth={1.5}
        />
      </svg>
    ) : null}
    {subtitle ? (
      <text
        x={54}
        y={26}
        fontSize={10}
        fill={isDark ? '#666' : '#737373'}
        fontWeight={600}
        letterSpacing={0.5}
        className="transition-colors duration-300"
      >
        {subtitle}
      </text>
    ) : null}
    <text
      x={54}
      y={subtitle ? 42 : 33}
      fontSize={12}
      fill={isDark ? '#EDEDED' : '#171717'}
      fontWeight={500}
      className="transition-colors duration-300"
    >
      {title}
    </text>
  </g>
)

const ArchitectureDiagram = ({ isDark }: { isDark: boolean }) => {
  const [isCompact, setIsCompact] = useState(() => window.matchMedia('(max-width: 767px)').matches)

  useEffect(() => {
    const mediaQuery = window.matchMedia('(max-width: 767px)')
    const handler = (event: MediaQueryListEvent) => setIsCompact(event.matches)
    mediaQuery.addEventListener('change', handler)
    return () => mediaQuery.removeEventListener('change', handler)
  }, [])

  const clientWidth = isCompact ? 160 : 220
  const clientRight = 20 + clientWidth
  const consoleX = isCompact ? 300 : 320
  const consoleRight = consoleX + 120
  const clientMid = Math.round((clientRight + consoleX) / 2)
  const environmentX = isCompact ? 540 : 820
  const environmentWidth = isCompact ? 160 : 220
  const nextX = isCompact ? environmentX : 520
  const consoleMid = Math.round((consoleRight + nextX) / 2)

  return (
    <div className="relative w-full overflow-visible">
      <div
        className={`absolute top-0 left-0 z-20 text-[10px] font-mono tracking-wider uppercase ${
          isDark ? 'text-neutral-500' : 'text-neutral-400'
        } transition-colors duration-300`}
      >
        Architecture
      </div>
      <div
        className={`absolute inset-0 transition-opacity duration-300 ${
          isDark ? 'opacity-[0.15]' : 'opacity-[0.4]'
        }`}
        style={{
          backgroundImage: `radial-gradient(${isDark ? '#444' : '#D4D4D4'} 1px, transparent 1px)`,
          backgroundSize: '24px 24px',
        }}
      />
      <div
        className={`pointer-events-none absolute inset-0 z-0 bg-linear-to-r ${
          isDark ? 'from-black via-transparent to-black' : 'from-white via-transparent to-white'
        } transition-colors duration-300`}
      />
      <div
        className={`pointer-events-none absolute inset-0 z-0 bg-linear-to-b ${
          isDark ? 'from-black via-transparent to-black' : 'from-white via-transparent to-white'
        } transition-colors duration-300`}
      />

      <svg
        viewBox={isCompact ? '0 0 720 400' : '0 0 1060 400'}
        className="relative z-10 h-auto w-full overflow-visible"
      >
        <defs>
          <filter id="shadow-sm" x="-10%" y="-10%" width="120%" height="120%">
            <feDropShadow dx="0" dy="2" stdDeviation="4" floodColor="#000" floodOpacity="0.05" />
          </filter>
          <filter id="shadow-lg" x="-20%" y="-20%" width="140%" height="140%">
            <feDropShadow dx="0" dy="8" stdDeviation="16" floodColor="#000" floodOpacity="0.08" />
          </filter>
          <filter id="shadow-dark" x="-10%" y="-10%" width="120%" height="120%">
            <feDropShadow dx="0" dy="2" stdDeviation="4" floodColor="#000" floodOpacity="0.3" />
          </filter>
          <filter id="shadow-dark-lg" x="-20%" y="-20%" width="140%" height="140%">
            <feDropShadow dx="0" dy="8" stdDeviation="16" floodColor="#000" floodOpacity="0.4" />
          </filter>
        </defs>

        <FlowLine isDark={isDark} d={`M ${clientRight} 108 C ${clientMid} 108, ${clientMid} 188, ${consoleX} 188`} />
        <FlowLine isDark={isDark} d={`M ${clientRight} 188 L ${consoleX} 188`} />
        <FlowLine isDark={isDark} d={`M ${clientRight} 268 C ${clientMid} 268, ${clientMid} 188, ${consoleX} 188`} />

        <FlowLine isDark={isDark} d={`M ${consoleRight} 188 C ${consoleMid} 188, ${consoleMid} 108, ${nextX} 108`} />
        <FlowLine isDark={isDark} d={`M ${consoleRight} 188 L ${nextX} 188`} />
        <FlowLine isDark={isDark} d={`M ${consoleRight} 188 C ${consoleMid} 188, ${consoleMid} 268, ${nextX} 268`} />

        {!isCompact ? (
          <>
            <FlowLine isDark={isDark} d="M 740 108 L 820 108" />
            <FlowLine isDark={isDark} d="M 740 188 L 820 188" />
            <FlowLine isDark={isDark} d="M 740 268 L 820 268" />
          </>
        ) : null}

        <SvgNode isDark={isDark} x={20} y={80} width={clientWidth} title="Developer" subtitle="Client" icon={User} />
        <SvgNode isDark={isDark} x={20} y={160} width={clientWidth} title="API Client" subtitle="Client" icon={Terminal} />
        <SvgNode isDark={isDark} x={20} y={240} width={clientWidth} title="MCP Client" subtitle="Client" icon={Blocks} active />

        <g
          transform={`translate(${consoleX}, 130)`}
          filter={isDark ? 'url(#shadow-dark-lg)' : 'url(#shadow-lg)'}
          className="transition-all duration-300"
        >
          <rect
            width={120}
            height={116}
            rx={16}
            fill={isDark ? '#141414' : '#FFFFFF'}
            stroke={isDark ? '#333' : '#E5E5E5'}
            strokeWidth={1}
            className="transition-colors duration-300"
          />
          <rect
            x={36}
            y={24}
            width={48}
            height={48}
            rx={12}
            fill={isDark ? '#1A1A1A' : '#F5F5F5'}
            stroke={isDark ? '#444' : '#E5E5E5'}
            strokeWidth={1}
            className="transition-colors duration-300"
          />
          <svg x={48} y={36} width={24} height={24}>
            <Server
              size={24}
              className={`${isDark ? 'text-white' : 'text-black'} transition-colors duration-300`}
              strokeWidth={1.5}
            />
          </svg>
          <text
            x={60}
            y={96}
            textAnchor="middle"
            fontSize={14}
            fill={isDark ? '#EDEDED' : '#171717'}
            fontWeight={600}
            letterSpacing={0.5}
            className="transition-colors duration-300"
          >
            Console
          </text>
        </g>

        {!isCompact ? (
          <>
            <SvgNode isDark={isDark} x={520} y={80} title="Worker Node" subtitle="Execution" icon={Cpu} />
            <SvgNode isDark={isDark} x={520} y={160} title="Worker Node" subtitle="Execution" icon={Cpu} />
            <SvgNode isDark={isDark} x={520} y={240} title="Worker Node" subtitle="Execution" icon={Cpu} />
          </>
        ) : null}

        <SvgNode isDark={isDark} x={environmentX} y={80} width={environmentWidth} title="Docker" subtitle="Environment" icon={Box} />
        <SvgNode
          isDark={isDark}
          x={environmentX}
          y={160}
          width={environmentWidth}
          title="Boxlite"
          subtitle="Environment"
          icon={Package}
          dashed
        />
        <SvgNode
          isDark={isDark}
          x={environmentX}
          y={240}
          width={environmentWidth}
          title="OS Process"
          subtitle="Environment"
          icon={Terminal}
          dashed
        />

        <g
          fill={isDark ? '#0A0A0A' : '#FFFFFF'}
          stroke={isDark ? '#555' : '#A3A3A3'}
          strokeWidth={1.5}
          className="transition-colors duration-300"
        >
          <circle cx={clientRight} cy={108} r={3} />
          <circle cx={clientRight} cy={188} r={3} />
          <circle cx={clientRight} cy={268} r={3} />
          <circle cx={consoleX} cy={188} r={3} />
          <circle cx={consoleRight} cy={188} r={3} />
          <circle cx={nextX} cy={108} r={3} />
          <circle cx={nextX} cy={188} r={3} />
          <circle cx={nextX} cy={268} r={3} />
          {!isCompact ? (
            <>
              <circle cx={740} cy={108} r={3} />
              <circle cx={740} cy={188} r={3} />
              <circle cx={740} cy={268} r={3} />
              <circle cx={820} cy={108} r={3} />
              <circle cx={820} cy={188} r={3} />
              <circle cx={820} cy={268} r={3} />
            </>
          ) : null}
        </g>
      </svg>
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
  const [isDark, setIsDark] = useState(() => window.matchMedia('(prefers-color-scheme: dark)').matches)

  useEffect(() => {
    const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)')
    const handler = (event: MediaQueryListEvent) => setIsDark(event.matches)
    mediaQuery.addEventListener('change', handler)
    return () => mediaQuery.removeEventListener('change', handler)
  }, [])

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
          <Box className={`h-5 w-5 ${isDark ? 'text-white' : 'text-black'} transition-colors duration-300`} />
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
        </div>
      </header>

      <main className="flex min-h-[calc(100vh-65px)] flex-1 flex-col justify-center px-6 py-12 lg:px-12 xl:px-20">
        <div className="flex flex-col items-center gap-12 xl:flex-row xl:gap-20">
          <div className="z-10 flex w-full shrink-0 flex-col justify-center xl:w-[450px]">
            <RotatingText />

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
              A self-hosted code execution sandbox platform for individuals and small teams.
            </p>

            <div className="flex flex-col items-start gap-4 sm:flex-row sm:items-center">
              <Link
                to="/docs"
                className={`flex w-full items-center justify-center gap-2 rounded-md px-6 py-3 font-medium shadow-md transition-colors sm:w-auto ${
                  isDark ? 'bg-white text-black hover:bg-neutral-200' : 'bg-black text-white hover:bg-neutral-800'
                }`}
              >
                <BookOpen className="h-4 w-4" />
                Documentation
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
                GitHub
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
              <ArchitectureDiagram isDark={isDark} />
            </div>
          </div>
        </div>

        <InstallCommand isDark={isDark} />
      </main>
    </div>
  )
}

export default HomePage
