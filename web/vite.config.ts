/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// In dev the browser hits the Vite server on :5173 and these proxy rules forward
// the API + websocket to the node's control plane on :8443 (mTLS — README §4).
// The browser authenticates by admin session/API key, not a client cert, so the
// proxy uses `secure: false` to accept the node's self-signed dev cert. The /ws
// proxy is dormant in the MVP (live status is ~1 Hz polling of GET
// /api/v1/groups/{id}/status — 08 G.2 / 09 §0) but is wired so a future stream
// can be enabled without touching the build.
// In prod, `vite build` emits to internal/web/dist, which Go embeds and serves.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'https://127.0.0.1:8443', secure: false, changeOrigin: true },
      '/ws': { target: 'https://127.0.0.1:8443', secure: false, ws: true, changeOrigin: true },
    },
  },
  // Vitest resolves Svelte's *browser* build (not the SSR build) so component
  // tests mount in jsdom.
  resolve: {
    conditions: process.env.VITEST ? ['browser'] : [],
  },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.ts'],
  },
})
