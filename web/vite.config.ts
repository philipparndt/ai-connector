import path from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { homeUi } from "@mqtt-home/ui/vite";

// The build goes into the Go server's embedded folder; `pnpm dev` proxies the API to a running mlxlab
// (MLXLAB_URL, default the port `make lan` uses).
const api = process.env.MLXLAB_URL ?? "http://127.0.0.1:8484";

export default defineConfig({
  // homeUi(): the prepaint script (dark mode, Wall Display detection) first in <head>
  plugins: [react(), tailwindcss(), homeUi()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  build: { outDir: "../internal/webui/dist", emptyOutDir: true },
  server: {
    proxy: {
      "/api": { target: api, changeOrigin: false },
      "/files": { target: api, changeOrigin: false },
    },
  },
});
