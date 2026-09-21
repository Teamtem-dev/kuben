import { fileURLToPath } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'
import { cspStyles } from './vite-plugins/csp-styles'

const src = (path: string) => fileURLToPath(new URL(`./src/${path}`, import.meta.url))

export default defineConfig({
  plugins: [react(), tailwindcss(), cspStyles()],
  resolve: {
    alias: [
      // Scroll locking (Radix Dialog, Sheet, menus) through constructed
      // stylesheets instead of <style> elements: see src/lib/style-singleton.ts.
      { find: /^react-style-singleton$/, replacement: src('lib/style-singleton.ts') },
      { find: /^@\//, replacement: `${src('')}` },
    ],
  },
  server: {
    port: 5173,
    strictPort: true,
    // The API sets `__Host-` cookies; keep the browser on one origin in dev.
    proxy: { '/api': { target: 'http://127.0.0.1:8080', ws: true } },
  },
  build: {
    target: 'es2022',
    sourcemap: false,
    // Output is embedded into the binary (go:embed): no inlined assets, so
    // the strict CSP (`img-src 'self' data:`) stays the only allowance.
    assetsInlineLimit: 0,
  },
})
