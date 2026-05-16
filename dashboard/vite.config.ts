import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

const apiPort = process.env.API_PORT ?? '8081'
const apiTarget = `http://localhost:${apiPort}`

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 8092,
    proxy: {
      '/ws': { target: apiTarget, ws: true },
      // ws: true is required for the Gemini Live bridge at
      // /api/voice/live — without it Vite silently drops the upgrade
      // handshake and the browser fires WebSocket.onerror immediately.
      // Plain HTTP requests on /api/* are unaffected (http-proxy only
      // applies the ws flag to actual Upgrade requests).
      '/api': { target: apiTarget, ws: true },
      '/health': apiTarget,
    },
  },
})
