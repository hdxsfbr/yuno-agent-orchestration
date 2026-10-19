import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";

// In `make dev` the Vite server runs in a container and proxies to the api service.
const apiTarget = process.env.API_URL ?? "http://api:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    host: true,
    port: 5173,
    proxy: {
      "/api": { target: apiTarget, changeOrigin: false },
      "/webhooks": { target: apiTarget, changeOrigin: false },
      "/openapi.yaml": { target: apiTarget, changeOrigin: false },
      "/deploy": { target: process.env.DEPLOY_URL ?? "http://deploytarget:8091", rewrite: (p) => p.replace(/^\/deploy/, "") },
    },
  },
  build: { outDir: "dist", sourcemap: false },
  test: { environment: "node", include: ["src/**/*.test.ts", "src/**/*.test.tsx"], setupFiles: ["src/test/setup.ts"], testTimeout: 30000 },
});
