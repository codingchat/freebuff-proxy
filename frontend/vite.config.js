import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { resolve } from 'path';

const proxyTarget = {
  target: 'http://127.0.0.1:3457',
  changeOrigin: true,
  configure: (proxy) => {
    proxy.on('proxyReq', (proxyReq) => {
      proxyReq.setHeader('Origin', 'http://127.0.0.1:3457');
    });
  },
};

function adminRedirectPlugin() {
  return {
    name: 'admin-redirect',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = req.url || '';
        if (url === '/admin') {
          res.writeHead(302, { Location: '/admin/' });
          res.end();
          return;
        }
        if (url.startsWith('/admin?')) {
          res.writeHead(302, { Location: '/admin/' + url.slice(6) });
          res.end();
          return;
        }
        if (url === '/' || url === '') {
          res.writeHead(302, { Location: '/admin/' });
          res.end();
          return;
        }
        next();
      });
    },
  };
}

export default defineConfig({
  plugins: [
    adminRedirectPlugin(),
    tailwindcss(),
    svelte(),
  ],
  base: '/admin/',
  build: {
    outDir: resolve(__dirname, '../internal/dashboard/dist'),
    emptyOutDir: true,
    assetsDir: 'assets',
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    // Dev-only dashboard server. Loopback bind + explicit Host allowlist:
    // the /admin/* proxy reaches the real gateway at 127.0.0.1:3457, whose
    // adminSensitive endpoints trust the vite proxy's loopback RemoteAddr.
    // Binding 0.0.0.0 or accepting any Host (allowedHosts:true) would let LAN
    // peers / DNS-rebinding hosts read the full .env when ADMIN_TOKEN is unset.
    allowedHosts: ['127.0.0.1', 'localhost'],
    proxy: {
      '/admin/api': proxyTarget,
      '/admin/login': proxyTarget,
      '/admin/smoke': proxyTarget,
      '/admin/diag': proxyTarget,
      '/admin/tokens': proxyTarget,
      '/admin/mode': proxyTarget,
      '/admin/config': proxyTarget,
      '/admin/playground': proxyTarget,
      '/healthz': proxyTarget,
      '/metrics': proxyTarget,
    },
  },
});
