import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The interface is served by the Go binary under /app/, embedded rather than
// copied beside it. Two consequences are load-bearing here.
//
// base must match the mount point: an absolute asset path of /assets/… would
// 404 behind /app/, and the router's fallback would answer it with index.html,
// which the browser then refuses to execute as JavaScript.
//
// No inline assets and no inline styles. The document's Content-Security-Policy
// is script-src 'self'; style-src 'self' — an inlined data: script or a
// <style> block is a blank screen in production and a working one in dev,
// which is the worst failure to discover.
export default defineConfig({
  base: "/app/",
  plugins: [react()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    assetsInlineLimit: 0,
    cssCodeSplit: false,
    sourcemap: false,
    target: "es2022",
  },
  server: {
    port: 5273,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: true },
    },
  },
});
