import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig } from 'vite'
import path from 'node:path'

// Embedded in the easyops binary and served at /ui/ (see cmd/easyops/ui.go).
// The ops.v1 Connect API is same-origin (/easyops.v1.OpsService/*).
export default defineConfig({
  base: '/ui/',
  plugins: [tailwindcss(), svelte()],
  resolve: { alias: { $lib: path.resolve('./src/lib') } },
  build: { outDir: '../cmd/easyops/ui/dist', emptyOutDir: true, target: 'es2022' },
})
