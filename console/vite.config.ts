import path from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { homeUi } from "@mqtt-home/ui/vite";

// The console of ai-connector, served by the server under /console/ (internal/consoleui embeds the build).
// `pnpm dev` proxies the APIs to a running server (MLXLAB_URL, default the port `make serve` uses).
const api = process.env.MLXLAB_URL ?? "http://127.0.0.1:8484";

export default defineConfig({
  base: "/console/",
  // homeUi(): the prepaint script (dark mode, Wall Display detection) first in <head>
  plugins: [react(), tailwindcss(), homeUi()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "src") } },
  build: { outDir: "../internal/consoleui/dist", emptyOutDir: true },
  server: {
    proxy: {
      "/api": { target: api, changeOrigin: false },
      "/files": { target: api, changeOrigin: false },
      "/v1": { target: api, changeOrigin: false },
    },
  },
});
