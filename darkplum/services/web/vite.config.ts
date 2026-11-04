import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// `pnpm dev` proxies API calls to the local stack. The stack is on port 8080
// unless `scripts/dev.sh up` was given DEV_PORT, in which case start the dev
// server with the same value: DEV_PORT=8090 pnpm dev. The address is written
// as 127.0.0.1, not localhost: Node resolves localhost to ::1 first, and kind
// publishes the port on IPv4, so every proxied call would wait out the IPv6
// attempt before falling back.
const devStack = `http://127.0.0.1:${process.env.DEV_PORT ?? '8080'}`

export default defineConfig({
  plugins: [react()],
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          markdown: ['react-markdown', 'remark-gfm', 'rehype-highlight', 'highlight.js'],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': {
        target: devStack,
        changeOrigin: true,
      },
      '/v1': {
        target: devStack,
        changeOrigin: true,
      },
      '/auth': {
        target: devStack,
        changeOrigin: true,
      },
    },
  },
})
