import { readdirSync, readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { fileURLToPath, URL } from 'node:url'
import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import compression from 'vite-plugin-compression'

// pdf.js fetches some files by name while it renders a chat PDF preview
// (src/lib/pdfPreview.ts): CMaps for CJK text, font data for Symbol and
// ZapfDingbats, and the JavaScript decoders for JBIG2/CCITT scans and JPEG
// 2000 images. Hashed names would not match, so they ship under fixed names
// in a directory named after the pdf.js version; an upgrade never reads an
// older copy.
function pdfjsRuntimeFiles(): Plugin {
  const root = dirname(createRequire(import.meta.url).resolve('pdfjs-dist/package.json'))
  const { version } = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')) as { version: string }
  const files = [
    ...readdirSync(join(root, 'cmaps')).map(name => `cmaps/${name}`),
    'standard_fonts/FoxitDingbats.pfb',
    'standard_fonts/FoxitSymbol.pfb',
    'standard_fonts/LICENSE_FOXIT',
    'wasm/jbig2_nowasm_fallback.js',
    'wasm/openjpeg_nowasm_fallback.js',
    'wasm/LICENSE_JBIG2',
    'wasm/LICENSE_OPENJPEG',
    'wasm/LICENSE_PDFJS_JBIG2',
    'wasm/LICENSE_PDFJS_OPENJPEG',
  ]
  let assetsDir = 'assets'
  return {
    name: 'pdfjs-runtime-files',
    apply: 'build',
    configResolved(config) {
      assetsDir = config.build.assetsDir
    },
    generateBundle() {
      for (const file of files) {
        this.emitFile({
          type: 'asset',
          fileName: `${assetsDir}/pdfjs-${version}/${file}`,
          source: readFileSync(join(root, file)),
        })
      }
    },
  }
}

export default defineConfig({
  base: './',
  plugins: [
    vue(),
    pdfjsRuntimeFiles(),
    // Gzip compression
    compression({
      algorithm: 'gzip',
      ext: '.gz',
      threshold: 1024 // Only compress files > 1KB
    }),
    // Brotli compression (better ratio)
    compression({
      algorithm: 'brotliCompress',
      ext: '.br',
      threshold: 1024
    })
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    }
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          // Core Vue ecosystem
          'vue-vendor': ['vue', 'vue-router', 'pinia'],
          // UI primitives
          'reka-ui': ['reka-ui'],
          // Charts (heavy)
          'charts': ['chart.js', 'vue-chartjs'],
          // Grid layout (heavy)
          'grid-layout': ['grid-layout-plus'],
          // Emoji picker (heavy)
          'emoji-picker': ['vue3-emoji-picker'],
          // Form validation
          'validation': ['vee-validate', '@vee-validate/zod', 'zod'],
          // Utilities
          'utils': ['@vueuse/core', 'axios', 'clsx', 'tailwind-merge', 'class-variance-authority']
        }
      }
    },
    // Increase chunk size warning limit (optional)
    chunkSizeWarningLimit: 500
  },
  // Drop console and debugger in production builds
  esbuild: {
    drop: ['console', 'debugger']
  },
  server: {
    port: 3000,
    allowedHosts: [],
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true
      },
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true
      }
    }
  }
})
