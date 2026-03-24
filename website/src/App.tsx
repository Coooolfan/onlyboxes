import { useState, useEffect } from 'react'
import { Github, BookOpen, Copy, Check, Terminal, Blocks, Server, Cpu, Box, Package, User, Sun, Moon } from 'lucide-react'

const RotatingText = ({ isDark }: { isDark: boolean }) => {
  const texts = [
    "Run LLM-generated code safely?",
    "Build a sandbox service?"
  ];
  const [index, setIndex] = useState(0);
  const [fade, setFade] = useState(true);

  useEffect(() => {
    const interval = setInterval(() => {
      setFade(false);
      setTimeout(() => {
        setIndex((current) => (current + 1) % texts.length);
        setFade(true);
      }, 500);
    }, 3000);

    return () => clearInterval(interval);
  }, []);

  return (
    <div className="h-6 mb-4 flex items-center">
      <span 
        className={`text-sm ${isDark ? 'text-neutral-500' : 'text-neutral-500'} transition-opacity duration-500 ease-in-out ${fade ? 'opacity-100' : 'opacity-0'}`}
      >
        {texts[index]}
      </span>
    </div>
  );
};

const FlowLine = ({ d, isDark }: { d: string; isDark: boolean }) => (
  <g>
    <path d={d} stroke={isDark ? "#222" : "#E5E5E5"} strokeWidth="1.5" fill="none" className="transition-colors duration-300" />
    <path d={d} stroke={isDark ? "#FFF" : "#000"} strokeWidth="1.5" strokeDasharray="4 8" fill="none" className="transition-colors duration-300">
      <animate attributeName="stroke-dashoffset" from="12" to="0" dur="1s" repeatCount="indefinite" />
    </path>
  </g>
);

interface SvgNodeProps {
  x: number;
  y: number;
  width?: number;
  height?: number;
  title: string;
  subtitle?: string;
  icon?: React.ElementType;
  dashed?: boolean;
  active?: boolean;
  isDark: boolean;
}

const SvgNode = ({ x, y, width = 220, height = 56, title, subtitle, icon: Icon, dashed = false, active = false, isDark }: SvgNodeProps) => (
  <g transform={`translate(${x}, ${y})`} filter={isDark ? "url(#shadow-dark)" : "url(#shadow-sm)"} className="transition-all duration-300">
    <rect 
      width={width} height={height} rx={8} 
      fill={isDark ? "#141414" : "#FFFFFF"} 
      stroke={active ? (isDark ? "#555" : "#A3A3A3") : (isDark ? "#2A2A2A" : "#E5E5E5")} 
      strokeWidth={1} strokeDasharray={dashed ? "4 4" : "none"} 
      className="transition-colors duration-300"
    />
    <rect 
      x={12} y={12} width={32} height={32} rx={6} 
      fill={isDark ? "#1A1A1A" : "#F5F5F5"} 
      stroke={isDark ? "#2A2A2A" : "#E5E5E5"} 
      strokeWidth={1} 
      className="transition-colors duration-300"
    />
    {Icon && (
      <svg x={16} y={16} width={24} height={24}>
        <Icon size={24} className={`${isDark ? "text-white" : "text-black"} transition-colors duration-300`} strokeWidth={1.5} />
      </svg>
    )}
    {subtitle && <text x={54} y={26} fontSize={10} fill={isDark ? "#666" : "#737373"} fontWeight={600} letterSpacing={0.5} className="transition-colors duration-300">{subtitle}</text>}
    <text x={54} y={subtitle ? 42 : 33} fontSize={12} fill={isDark ? "#EDEDED" : "#171717"} fontWeight={500} className="transition-colors duration-300">{title}</text>
  </g>
);

const ArchitectureDiagram = ({ isDark }: { isDark: boolean }) => {
  return (
    <div className="w-full relative overflow-visible">
      {/* Background Grid Pattern */}
      <div 
        className={`absolute inset-0 transition-opacity duration-300 ${isDark ? 'opacity-[0.15]' : 'opacity-[0.4]'}`} 
        style={{ backgroundImage: `radial-gradient(${isDark ? '#444' : '#D4D4D4'} 1px, transparent 1px)`, backgroundSize: '24px 24px' }}
      ></div>
      
      {/* Fade out edges */}
      <div className={`absolute inset-0 bg-linear-to-r ${isDark ? 'from-black via-transparent to-black' : 'from-white via-transparent to-white'} z-0 pointer-events-none transition-colors duration-300`}></div>
      <div className={`absolute inset-0 bg-linear-to-b ${isDark ? 'from-black via-transparent to-black' : 'from-white via-transparent to-white'} z-0 pointer-events-none transition-colors duration-300`}></div>

      <svg viewBox="0 0 1060 400" className="w-full h-auto relative z-10 overflow-visible">
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

        {/* Lines */}
        <FlowLine isDark={isDark} d="M 240 108 C 280 108, 280 188, 320 188" />
        <FlowLine isDark={isDark} d="M 240 188 L 320 188" />
        <FlowLine isDark={isDark} d="M 240 268 C 280 268, 280 188, 320 188" />
        
        <FlowLine isDark={isDark} d="M 440 188 C 480 188, 480 108, 520 108" />
        <FlowLine isDark={isDark} d="M 440 188 L 520 188" />
        <FlowLine isDark={isDark} d="M 440 188 C 480 188, 480 268, 520 268" />
        
        <FlowLine isDark={isDark} d="M 740 108 L 820 108" />
        <FlowLine isDark={isDark} d="M 740 188 L 820 188" />
        <FlowLine isDark={isDark} d="M 740 268 L 820 268" />

        {/* Nodes Col 1: Clients */}
        <SvgNode isDark={isDark} x={20} y={80} title="Developer" subtitle="Client" icon={User} />
        <SvgNode isDark={isDark} x={20} y={160} title="API Client" subtitle="Client" icon={Terminal} />
        <SvgNode isDark={isDark} x={20} y={240} title="MCP Client" subtitle="Client" icon={Blocks} active />

        {/* Node Col 2: Console Hub */}
        <g transform={`translate(320, 130)`} filter={isDark ? "url(#shadow-dark-lg)" : "url(#shadow-lg)"} className="transition-all duration-300">
          <rect width={120} height={116} rx={16} fill={isDark ? "#141414" : "#FFFFFF"} stroke={isDark ? "#333" : "#E5E5E5"} strokeWidth={1} className="transition-colors duration-300" />
          <rect x={36} y={24} width={48} height={48} rx={12} fill={isDark ? "#1A1A1A" : "#F5F5F5"} stroke={isDark ? "#444" : "#E5E5E5"} strokeWidth={1} className="transition-colors duration-300" />
          <svg x={48} y={36} width={24} height={24}>
            <Server size={24} className={`${isDark ? "text-white" : "text-black"} transition-colors duration-300`} strokeWidth={1.5} />
          </svg>
          <text x={60} y={96} textAnchor="middle" fontSize={14} fill={isDark ? "#EDEDED" : "#171717"} fontWeight={600} letterSpacing={0.5} className="transition-colors duration-300">Console</text>
        </g>

        {/* Nodes Col 3: Workers */}
        <SvgNode isDark={isDark} x={520} y={80} title="Worker Node" subtitle="Execution" icon={Cpu} />
        <SvgNode isDark={isDark} x={520} y={160} title="Worker Node" subtitle="Execution" icon={Cpu} />
        <SvgNode isDark={isDark} x={520} y={240} title="Worker Node" subtitle="Execution" icon={Cpu} />

        {/* Nodes Col 4: Environments */}
        <SvgNode isDark={isDark} x={820} y={80} title="Docker" subtitle="Environment" icon={Box} />
        <SvgNode isDark={isDark} x={820} y={160} title="Boxlite (WIP)" subtitle="Environment" icon={Package} dashed />
        <SvgNode isDark={isDark} x={820} y={240} title="OS Process" subtitle="Environment" icon={Terminal} dashed />

        {/* Anchors */}
        <g fill={isDark ? "#0A0A0A" : "#FFFFFF"} stroke={isDark ? "#555" : "#A3A3A3"} strokeWidth={1.5} className="transition-colors duration-300">
          <circle cx={240} cy={108} r={3} />
          <circle cx={240} cy={188} r={3} />
          <circle cx={240} cy={268} r={3} />
          
          <circle cx={320} cy={188} r={3} />
          <circle cx={440} cy={188} r={3} />
          
          <circle cx={520} cy={108} r={3} />
          <circle cx={520} cy={188} r={3} />
          <circle cx={520} cy={268} r={3} />
          
          <circle cx={740} cy={108} r={3} />
          <circle cx={740} cy={188} r={3} />
          <circle cx={740} cy={268} r={3} />

          <circle cx={820} cy={108} r={3} />
          <circle cx={820} cy={188} r={3} />
          <circle cx={820} cy={268} r={3} />
        </g>
      </svg>
      
      {/* Labels */}
      <div className="absolute top-0 left-0 flex gap-2 z-20">
        <div className={`flex items-center gap-1.5 text-[10px] text-neutral-500 font-mono tracking-wider uppercase ${isDark ? 'bg-black/50 border-neutral-800' : 'bg-white/80 border-neutral-200'} px-2 py-1 rounded-md border backdrop-blur-sm shadow-sm transition-colors duration-300`}>
           <div className={`w-1.5 h-1.5 rounded-full ${isDark ? 'bg-white' : 'bg-black'} animate-pulse transition-colors duration-300`}></div>
           Control Plane
        </div>
        <div className={`flex items-center gap-1.5 text-[10px] text-neutral-500 font-mono tracking-wider uppercase ${isDark ? 'bg-black/50 border-neutral-800' : 'bg-white/80 border-neutral-200'} px-2 py-1 rounded-md border backdrop-blur-sm shadow-sm transition-colors duration-300`}>
           Execution Plane
        </div>
      </div>
    </div>
  );
};

const INSTALL_CMD = 'curl -fsSL https://onlybox.es/install.sh | bash -s -- --tag 0.1.5';

const InstallCommand = ({ isDark }: { isDark: boolean }) => {
  const [copied, setCopied] = useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(INSTALL_CMD);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className={`mt-6 w-fit max-w-full flex items-center rounded-lg border px-3 py-2.5 font-mono text-sm ${isDark ? 'bg-neutral-900/60 border-neutral-800 text-neutral-300' : 'bg-neutral-50 border-neutral-200 text-neutral-600'} transition-colors duration-300`}>
      <span className={`select-none mr-1.5 ${isDark ? 'text-neutral-600' : 'text-neutral-400'}`}>$</span>
      <code className="overflow-x-auto whitespace-nowrap min-w-0">{INSTALL_CMD}</code>
      <button
        onClick={handleCopy}
        className={`shrink-0 ml-2 p-1 rounded ${isDark ? 'hover:bg-neutral-800 text-neutral-500 hover:text-white' : 'hover:bg-neutral-200 text-neutral-400 hover:text-black'} transition-colors`}
        aria-label="Copy command"
      >
        {copied ? <Check className="w-4 h-4" /> : <Copy className="w-4 h-4" />}
      </button>
    </div>
  );
};

function App() {
  const [isDark, setIsDark] = useState(() => window.matchMedia('(prefers-color-scheme: dark)').matches);

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => setIsDark(e.matches);
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, []);

  return (
    <div className={`min-h-screen ${isDark ? 'bg-black text-white selection:bg-neutral-800' : 'bg-white text-black selection:bg-neutral-200'} font-sans flex flex-col relative overflow-hidden transition-colors duration-300`}>
      {/* Navbar */}
      <header className={`flex items-center justify-between px-6 py-4 border-b ${isDark ? 'border-neutral-900' : 'border-neutral-100'} transition-colors duration-300`}>
        <div className="flex items-center gap-2">
          <Box className={`w-5 h-5 ${isDark ? 'text-white' : 'text-black'} transition-colors duration-300`} />
          <span className="font-semibold text-sm tracking-tight">OnlyBoxes</span>
        </div>
        <div className="flex items-center gap-4">
          <button 
            onClick={() => setIsDark(!isDark)}
            className={`p-2 rounded-md ${isDark ? 'hover:bg-neutral-900 text-neutral-400 hover:text-white' : 'hover:bg-neutral-100 text-neutral-500 hover:text-black'} transition-colors duration-300`}
            aria-label="Toggle theme"
          >
            {isDark ? <Sun className="w-5 h-5" /> : <Moon className="w-5 h-5" />}
          </button>
        </div>
      </header>

      {/* Main Content */}
      <main className="flex-1 flex flex-col justify-center w-full px-6 lg:px-12 xl:px-20 py-12 min-h-[calc(100vh-65px)]">
        <div className="flex flex-col xl:flex-row items-center gap-12 xl:gap-20">
          {/* Left Column - Copy */}
          <div className="w-full xl:w-[450px] shrink-0 z-10 flex flex-col justify-center">
            <RotatingText isDark={isDark} />
            
            <h1 className={`text-6xl md:text-7xl lg:text-8xl font-bold tracking-tighter mb-6 ${isDark ? 'text-white' : 'text-black'} leading-none transition-colors duration-300`}>
              OnlyBoxes
            </h1>
            
            <p className={`text-lg ${isDark ? 'text-neutral-400' : 'text-neutral-500'} mb-10 leading-relaxed font-light transition-colors duration-300`}>
              A self-hosted code execution sandbox platform for individuals and small teams.
            </p>
            
            <div className="flex flex-col sm:flex-row items-start sm:items-center gap-4">
              <a href="https://github.com/Coooolfan/onlyboxes#readme" target="_blank" rel="noopener noreferrer" className={`w-full sm:w-auto flex items-center justify-center gap-2 ${isDark ? 'bg-white text-black hover:bg-neutral-200' : 'bg-black text-white hover:bg-neutral-800'} px-6 py-3 rounded-md font-medium transition-colors shadow-md`}>
                <BookOpen className="w-4 h-4" />
                Documentation
              </a>
              <a href="https://github.com/Coooolfan/onlyboxes" target="_blank" rel="noopener noreferrer" className={`w-full sm:w-auto flex items-center justify-center gap-2 bg-transparent ${isDark ? 'text-white border-neutral-700 hover:bg-neutral-900' : 'text-black border-neutral-200 hover:bg-neutral-50 hover:border-neutral-300'} border px-6 py-3 rounded-md font-medium transition-all`}>
                <Github className="w-4 h-4" />
                GitHub
              </a>
            </div>
          </div>

          {/* Right Column - Visual */}
          <div className="w-full xl:w-auto flex-1 relative flex items-center justify-center min-w-0">
            {/* Subtle background glow for dark mode */}
            <div className={`absolute inset-0 bg-neutral-900/20 rounded-full blur-3xl -z-10 transition-opacity duration-500 ${isDark ? 'opacity-100' : 'opacity-0'}`}></div>
            
            <div className="w-full max-w-[1000px] relative">
               <ArchitectureDiagram isDark={isDark} />
            </div>
          </div>
        </div>

        {/* Install Command - Full Width */}
        <InstallCommand isDark={isDark} />
      </main>
    </div>
  );
}

export default App
