import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const backendTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:18080";

export default defineConfig({
  plugins: [react()],
  root: "web",
  build: {
    outDir: "../internal/ui/dist",
    emptyOutDir: true
  },
  server: {
    host: "0.0.0.0",
    port: 5173,
    proxy: {
      "/api": {
        target: backendTarget,
        changeOrigin: true,
        ws: true
      },
      "/healthz": backendTarget,
      "/readyz": backendTarget
    }
  }
});
