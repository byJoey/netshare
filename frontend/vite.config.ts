import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  base: "/assets/",
  build: {
    outDir: "../internal/webui/assets",
    emptyOutDir: true,
    assetsDir: "static",
    sourcemap: false,
  },
  server: { proxy: { "/api": "http://127.0.0.1:18440" } },
});
