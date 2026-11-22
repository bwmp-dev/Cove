import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { loadEnv, type ProxyOptions } from "vite";
import { defineConfig } from "vitest/config";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "COVE_");
  const backend = env.COVE_DEV_BACKEND ?? "http://127.0.0.1:8080";
  const publicURL = env.COVE_DEV_PUBLIC_URL;

  // The control plane accepts state-changing requests only from COVE_PUBLIC_URL.
  // In development the browser origin is the Vite server, so the proxy presents
  // the configured public origin instead. This never affects production builds.
  const proxy: ProxyOptions = {
    target: backend,
    configure(server) {
      server.on("proxyReq", (request) => {
        if (publicURL && request.getHeader("origin")) {
          request.setHeader("origin", publicURL);
        }
      });
    },
  };

  return {
    plugins: [react(), tailwindcss()],
    build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
    server: { proxy: { "/api": proxy, "/auth": proxy } },
    test: {
      include: ["src/**/*.test.{ts,tsx}"],
      environment: "jsdom",
      globals: true,
      setupFiles: ["./src/test/setup.ts"],
    },
  };
});
