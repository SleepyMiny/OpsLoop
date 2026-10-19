import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// Static, offline-capable build: no CDN assets, fonts are bundled from @fontsource.
export default defineConfig({
  plugins: [react()],
  base: './',
  server: { host: '127.0.0.1', port: 5173, strictPort: false, proxy: { '/api': { target: 'http://127.0.0.1:8090' } } },
  preview: { host: '127.0.0.1', port: 4173 },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
});
