// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import react from '@vitejs/plugin-react';
import type { Connect, PreviewServer, ViteDevServer } from 'vite';
import { configDefaults, defineConfig } from 'vitest/config';

const GATEWAY = 'http://127.0.0.1:8091';

// Mirror the nginx path-split for local dev and E2E: bare "/" redirects to the
// portal, "/portal/*" is the SPA, and backend paths are proxied to the gateway.
function portalRootRedirect() {
  const redirect: Connect.NextHandleFunction = (req, res, next) => {
    if (req.url === '/') {
      res.statusCode = 302;
      res.setHeader('Location', '/portal/');
      res.end();
      return;
    }
    next();
  };
  return {
    name: 'portal-root-redirect',
    configureServer(server: ViteDevServer) {
      server.middlewares.use(redirect);
    },
    configurePreviewServer(server: PreviewServer) {
      server.middlewares.use(redirect);
    },
  };
}

const proxy = {
  '/api': GATEWAY,
  '/v1': GATEWAY,
  '/openai': GATEWAY,
  '/anthropic': GATEWAY,
  '/healthz': GATEWAY,
};

export default defineConfig({
  base: '/portal/',
  plugins: [react(), portalRootRedirect()],
  server: { host: '127.0.0.1', port: 4173, strictPort: true, proxy },
  preview: { host: '127.0.0.1', port: 4173, strictPort: true, proxy },
  build: {
    // The limit applies to every chunk, lazy ones included, and the largest
    // chunk by far is intentional: heic-to inlines the libheif WASM decoder
    // (~3.0 MB minified, both dist variants) and only loads on a HEIC upload
    // via the dynamic import in components/shared/imageAttach.ts. The eager
    // chunks (index/vendor/mui/highlight, split below) are all ≤ ~600 kB;
    // if the warning ever fires again, a chunk grew past the decoder — treat
    // that as a regression, don't raise this further.
    chunkSizeWarningLimit: 3100,
    rollupOptions: {
      output: {
        // Split the always-loaded vendor libraries out of the app chunk so a
        // portal-code change doesn't invalidate the (much larger, rarely
        // changing) vendor bytes in the browser cache. heic-to is NOT listed
        // here on purpose: it is only reached via dynamic import
        // (components/shared/imageAttach.ts) and stays its own lazy chunk.
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return undefined;
          if (id.includes('/heic-to/') || id.includes('/libheif')) return undefined;
          if (id.includes('/@mui/') || id.includes('/@emotion/')) return 'mui';
          // highlight.js is NOT split out further: it is CommonJS, and its
          // rollup interop helpers end up in the sibling chunk, which rollup
          // reports as "Circular chunk: highlight -> vendor -> highlight".
          return 'vendor';
        },
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['@testing-library/jest-dom/vitest', './src/vitest.setup.ts'],
    // Playwright specs under e2e*/ (e2e, e2e-capture, e2e-capture-ram, …) use
    // their own `test()` global and must not be picked up by Vitest's default
    // `*.spec.ts` include pattern. The `e2e*/**` glob covers every current and
    // future e2e directory.
    exclude: [...configDefaults.exclude, 'e2e*/**', '**/.worktrees/**'],
    // `test:coverage` (npm script) runs this with --coverage. sonar.sh
    // coverage / gate rely on the lcov report landing at
    // gateway/frontend/coverage/lcov.info (see sonar-project.properties'
    // sonar.javascript.lcov.reportPaths).
    coverage: {
      provider: 'v8',
      reporter: ['lcov', 'text-summary'],
      reportsDirectory: 'coverage',
      include: ['src/**'],
      exclude: ['src/**/*.d.ts', 'src/**/*.test.ts', 'src/**/*.test.tsx', 'src/vitest.setup.ts'],
    },
  },
});
