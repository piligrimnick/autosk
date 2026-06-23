import { defineConfig } from "vitest/config";
import { fileURLToPath, URL } from "node:url";

// Vitest config for the pure state-layer unit tests (reducer / selectors / ipc
// error normalisation). These need no browser and no daemon — they exercise the
// correctness-critical pure functions that drive the whole UI. The `@` alias
// mirrors vite.config.ts / tsconfig so test imports resolve identically.
export default defineConfig({
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
