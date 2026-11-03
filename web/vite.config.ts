/// <reference types="vitest/config" />
import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode, command }) => {
  const env = loadEnv(mode, process.cwd(), 'VITE_');
  // The dev bearer token must never reach a production bundle. Fail the build
  // loudly if one is configured; src/env.ts also ignores it unless DEV.
  if (command === 'build' && (env.VITE_DEV_TOKEN || process.env.VITE_DEV_TOKEN)) {
    throw new Error('VITE_DEV_TOKEN is set during a build. Unset it: a dev token must never be bundled.');
  }
  const proxy = {
    '/api': { target: env.VITE_PROXY_TARGET || 'http://localhost:18091', changeOrigin: false },
  };
  return {
    plugins: [react()],
    build: { sourcemap: false, target: 'es2022' },
    server: { port: 5173, proxy },
    preview: { port: 4173, proxy },
    test: {
      environment: 'jsdom',
      globals: true,
      setupFiles: ['./src/test/setup.ts'],
      css: false,
      // LAUN-112: a test may wait 5 s for one element (setup.ts), so the test limit must be larger than that.
      testTimeout: 20000,
      hookTimeout: 20000,
      include: ['src/**/*.test.{ts,tsx}'],
    },
  };
});
