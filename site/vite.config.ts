import react from "@vitejs/plugin-react"
import path from "path"
import { defineConfig, PluginOption } from "vite"
import { visualizer } from "rollup-plugin-visualizer"
import checker from "vite-plugin-checker"

const plugins: PluginOption[] = [
  react(),
  checker({
    typescript: true,
  }),
]

if (process.env.STATS !== undefined) {
  plugins.push(
    visualizer({
      filename: "./stats/index.html",
    }),
  )
}

export default defineConfig({
  plugins: plugins,
  publicDir: path.resolve(__dirname, "./static"),
  build: {
    outDir: path.resolve(__dirname, "./out"),
    // We need to keep the /bin folder and GITKEEP files
    emptyOutDir: false,
    // 'hidden' works like true except that the corresponding sourcemap comments in the bundled files are suppressed
    sourcemap: "hidden",
  },
  define: {
    "process.env": {
      NODE_ENV: process.env.NODE_ENV,
      STORYBOOK: process.env.STORYBOOK,
      INSPECT_XSTATE: process.env.INSPECT_XSTATE,
    },
  },
  server: {
    port: process.env.PORT ? Number(process.env.PORT) : 8080,
    proxy: {
      "/api": {
        ws: true,
        changeOrigin: true,
        target: process.env.CODER_HOST || "http://localhost:3000",
        secure: process.env.NODE_ENV === "production",
        configure: (proxy) => {
          // Vite does not catch socket errors, and stops the webserver.
          // As /startup-logs endpoint can return HTTP 4xx status, we need to embrace
          // Vite with a custom error handler to prevent from quitting.
          proxy.on("proxyReqWs", (proxyReq, req, socket) => {
            if (process.env.NODE_ENV === "development") {
              proxyReq.setHeader(
                "origin",
                process.env.CODER_HOST || "http://localhost:3000",
              )
            }

            socket.on("error", (error) => {
              console.error(error)
            })
          })
        },
      },
      "/swagger": {
        target: process.env.CODER_HOST || "http://localhost:3000",
        secure: process.env.NODE_ENV === "production",
      },
    },
  },
  resolve: {
    alias: {
      api: path.resolve(__dirname, "./src/api"),
      components: path.resolve(__dirname, "./src/components"),
      hooks: path.resolve(__dirname, "./src/hooks"),
      contexts: path.resolve(__dirname, "./src/contexts"),
      i18n: path.resolve(__dirname, "./src/i18n"),
      pages: path.resolve(__dirname, "./src/pages"),
      testHelpers: path.resolve(__dirname, "./src/testHelpers"),
      theme: path.resolve(__dirname, "./src/theme"),
      utils: path.resolve(__dirname, "./src/utils"),
      xServices: path.resolve(__dirname, "./src/xServices"),
    },
  },
  // https://github.com/vitejs/vite/issues/12434
  // needed to fix Vite error: "outdated optimize dep"
  optimizeDeps: {
    exclude: ["@mui_material"],
  },
})
