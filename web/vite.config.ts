import { fileURLToPath, URL } from 'node:url'
import { readFileSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import vueDevTools from 'vite-plugin-vue-devtools'

const apiTarget = process.env.VITE_API_TARGET || 'http://127.0.0.1:8089'
const consoleVersion = process.env.VITE_CONSOLE_VERSION?.trim()
const workerStartupDefaultTag = consoleVersion && consoleVersion !== 'dev' ? consoleVersion : '0.5.1'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    vue(),
    tailwindcss(),
    vueDevTools(),
    {
      name: 'onlyboxes-worker-startup-version',
      writeBundle(options) {
        const outDir =
          typeof options.dir === 'string' ? options.dir : fileURLToPath(new URL('./dist', import.meta.url))
        const scriptPath = resolve(outDir, 'static/worker-startup.sh')
        const script = readFileSync(scriptPath, 'utf8')
        const updated = script
          .replace(/^DEFAULT_TAG="[^"]*"/m, `DEFAULT_TAG="${workerStartupDefaultTag}"`)
          .replace(/Defaults to [^.\n]+(?:\.[^.\n]+)*\./, `Defaults to ${workerStartupDefaultTag}.`)
        writeFileSync(scriptPath, updated)
      },
    },
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5178,
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/mcp':{
        target: apiTarget,
        changeOrigin: true,
      }
    },
  },
})
