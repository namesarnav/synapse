/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";
import { fileURLToPath } from "node:url";

const dir = path.dirname(fileURLToPath(import.meta.url));

const api = process.env.SYNAPSE_API ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@protocol": path.resolve(dir, "../../packages/protocol/src/index.ts") } },
  server: {
    port: 5174,
    proxy: {
      "/api": { target: api, changeOrigin: true },
      "/hooks": { target: api, changeOrigin: true },
      "/ws": { target: api.replace("http", "ws"), ws: true },
    },
  },
  test: { environment: "jsdom", globals: true, setupFiles: ["./src/test-setup.ts"] },
});
