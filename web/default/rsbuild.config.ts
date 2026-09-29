import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig, loadEnv } from '@rsbuild/core'
import { pluginReact } from '@rsbuild/plugin-react'

const root = path.dirname(fileURLToPath(import.meta.url))

export default defineConfig(({ envMode }) => {
  const env = loadEnv({ mode: envMode, prefixes: ['VITE_'] })
  const serverUrl =
    process.env.VITE_REACT_APP_SERVER_URL ||
    env.rawPublicVars.VITE_REACT_APP_SERVER_URL ||
    'http://localhost:8080'
  const isProd = envMode === 'production'
  const proxyPaths = [
    '/admin',
    '/dashboard',
    '/login',
    '/oauth',
    '/v1',
    '/api',
    '/health',
    '/metrics',
  ] as const

  return {
    plugins: [pluginReact()],
    source: { entry: { index: './src/main.tsx' } },
    resolve: { alias: { '@': path.join(root, 'src') } },
    html: { template: './index.html' },
    server: {
      host: '0.0.0.0',
      port: 5173,
      proxy: Object.fromEntries(
        proxyPaths.map((prefix) => [prefix, { target: serverUrl, changeOrigin: true }]),
      ),
    },
    output: {
      distPath: { root: 'dist' },
      assetPrefix: '/web/',
      target: 'web',
      minify: isProd,
    },
    splitChunks: {
      preset: 'default',
      cacheGroups: {
        'vendor-react': {
          test: /node_modules[\\/](react|react-dom)[\\/]/,
          name: 'vendor-react',
          chunks: 'all',
          priority: 0,
          enforce: true,
        },
        'vendor-router': {
          test: /node_modules[\\/]@tanstack[\\/]/,
          name: 'vendor-router',
          chunks: 'all',
          priority: 0,
          enforce: true,
        },
      },
    },
    performance: {
      removeConsole: isProd ? ['log'] : false,
      buildCache: { cacheDigest: [process.env.VITE_REACT_APP_VERSION] },
    },
  }
})
