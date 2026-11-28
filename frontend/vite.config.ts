import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { loadEnv, type ProxyOptions } from "vite";
import { defineConfig } from "vitest/config";
import { devProxyPath, parseDevPublicURL } from "./src/lib/dev-proxy";

export default defineConfig(({ command, mode }) => {
  const env = loadEnv(mode, process.cwd(), "COVE_");
  const backend = env.COVE_DEV_BACKEND ?? "http://127.0.0.1:8080";
  // Only the dev server proxies, so a bad value must not break production builds.
  const publicURL = command === "serve" ? parseDevPublicURL(env.COVE_DEV_PUBLIC_URL) : null;

  // The control plane accepts state-changing requests only from the origin of COVE_PUBLIC_URL and serves /api and
  // /auth under its path. In development the browser origin is the Vite server at the root, so the proxy presents
  // the configured public origin and adds the path prefix. This never affects production builds.
  const proxy: ProxyOptions = {
    target: backend,
    rewrite: (path) => devProxyPath(path, publicURL),
    configure(server) {
      server.on("proxyReq", (request) => {
        if (publicURL && request.getHeader("origin")) {
          request.setHeader("origin", publicURL.origin);
        }
      });
    },
  };

  return {
    // Relative asset URLs resolve against the <base href> the control plane injects, so one build serves the
    // root and any COVE_PUBLIC_URL path prefix.
    base: "./",
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
