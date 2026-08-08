import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The panel is served from /_admin/ inside the burrowd binary, so asset URLs
// must be built with that prefix.
//
// During `npm run dev`, the API is proxied to burrowd's dedicated admin
// listener. Start the server with `-admin-addr 127.0.0.1:7002`: that listener
// answers on any Host, whereas the one on port 80 only serves the panel for
// the configured base domain.
export default defineConfig({
  base: "/_admin/",
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // The whole panel is a handful of components; one chunk keeps the
    // embedded asset list short and the first paint immediate.
    chunkSizeWarningLimit: 900,
  },
  server: {
    port: 5173,
    proxy: {
      "/_api": {
        target: "http://127.0.0.1:7002",
        changeOrigin: false,
      },
    },
  },
});
