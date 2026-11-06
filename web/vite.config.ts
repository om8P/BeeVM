// Vite builds the dashboard into dist/, which `beevmd serve` shows at /ui/.
// In development (`make ui-dev`) Vite serves it on :5173 and forwards API calls to beevmd on
// :8080, so the browser only ever talks to one origin (no CORS needed anywhere).
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

const beevmd = 'http://127.0.0.1:8080'

export default defineConfig({
  base: '/ui/',
  plugins: [react()],
  server: {
    host: '127.0.0.1',
    port: 5173,
    // Set by scripts/ui.sh when the code is on /mnt/c (Windows files: no change events in WSL).
    watch: process.env.BEEVM_UI_POLL ? { usePolling: true, interval: 300 } : undefined,
    proxy: { '/api': beevmd, '/invoke': beevmd, '/healthz': beevmd },
  },
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1500 },
  test: { environment: 'node', include: ['src/**/*.test.ts'] },
})
