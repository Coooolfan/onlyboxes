import { fileURLToPath, URL } from 'node:url'

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import vueDevTools from 'vite-plugin-vue-devtools'

const apiTarget = process.env.VITE_API_TARGET || 'http://127.0.0.1:8089'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    vue(),
    tailwindcss(),
    vueDevTools(),
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
