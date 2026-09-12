import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    strictPort: true,
    // The API sets `__Host-` cookies; keep the browser on one origin in dev.
    proxy: { '/api': { target: 'http://127.0.0.1:8080', ws: true } },
  },
  build: {
    target: 'es2022',
    sourcemap: false,
    // Output is embedded into the binary (rust-embed): no inlined assets, so
    // the strict CSP (`img-src 'self' data:`) stays the only allowance.
    assetsInlineLimit: 0,
  },
})
