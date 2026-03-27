import { defineConfig } from 'vitest/config'
import type { Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import mdx from '@mdx-js/rollup'
import tailwindcss from '@tailwindcss/vite'
import remarkGfm from 'remark-gfm'
import rehypeSlug from 'rehype-slug'

const mdxPlugin = mdx({
  remarkPlugins: [remarkGfm],
  rehypePlugins: [rehypeSlug],
}) as Plugin

mdxPlugin.enforce = 'pre'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    mdxPlugin,
    react({
      include: /\.(mdx|js|jsx|ts|tsx)$/,
    }),
    tailwindcss(),
  ],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test/setup.ts',
  },
})
