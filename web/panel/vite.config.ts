import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The panel is served by the Go binary at /panel/ and embedded from
// internal/panel/dist. `npm run dev` proxies the API to a local server.
export default defineConfig({
  base: '/panel/',
  plugins: [react()],
  build: {
    outDir: '../../internal/panel/dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 900,
  },
  server: {
    proxy: { '/api': 'http://localhost:8080', '^/(?!panel/).+\\.(m3u8|mp4|ts|m4s|mp3|aac)$': 'http://localhost:8080' },
  },
});
