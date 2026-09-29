import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Assets use relative URLs; the Go server injects <base href> for the
// configured base path, so the same build works at "/" or "/sql/".
export default defineConfig({
  base: "./",
  plugins: [react()],
  worker: { format: "es" },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "es2022",
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    host: "127.0.0.1",
    port: 5199,
    proxy: { "/api": { target: "http://127.0.0.1:18080", changeOrigin: false } },
  },
});
