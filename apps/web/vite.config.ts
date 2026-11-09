import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const apiProxyTarget = env.VITE_API_PROXY_TARGET || 'http://localhost:3000';
  const apiProxyPrefix = env.VITE_API_PROXY_PREFIX || '/api';
  const goProxyTarget = env.VITE_GO_API_PROXY_TARGET;
  const escapedPrefix = apiProxyPrefix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  return {
    plugins: [react()],
    server: {
      port: 5173,
      proxy: {
        // Opt-in migration. Profile PUT and other CRUD still belong to NestJS.
        ...(goProxyTarget
          ? {
              [`^${escapedPrefix}/(?:auth/login|agent-runs/(?:stream|preview/stream|[a-f0-9]{32}(?:/(?:cancel|tool-calls|metrics))?))(?:\\?.*)?$`]: {
                target: goProxyTarget,
                changeOrigin: true,
                rewrite: (path: string) => `/api${path.slice(apiProxyPrefix.length)}`,
              },
            }
          : {}),
        ...(goProxyTarget && env.VITE_GO_PUBLIC_PROXY_ENABLED === 'true'
          ? {
              [`^${escapedPrefix}/public/(?:agents/[^/]+(?:/chat/stream|/runs/[a-f0-9]{32}(?:/cancel)?)?|agent-runs/(?:stream|[a-f0-9]{32}(?:/cancel)?))(?:\\?.*)?$`]: {
                target: goProxyTarget,
                changeOrigin: true,
                rewrite: (path: string) => `/api${path.slice(apiProxyPrefix.length)}`,
              },
            }
          : {}),
        [apiProxyPrefix]: {
          target: apiProxyTarget,
          changeOrigin: true,
        },
      },
    },
    preview: {
      port: 4173,
    },
    build: {
      rollupOptions: {
        output: {
          manualChunks(id) {
            if (id.includes('react-router')) return 'router';
            if (id.includes('node_modules')) return 'vendor';
          },
        },
      },
    },
  };
});
