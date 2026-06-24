// @ts-check
import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";

// Static, zero-backend marketing site. `site` is used for absolute OG/canonical
// URLs; update it once the hosting/domain decision (plan §10) is made.
export default defineConfig({
  site: "https://autosk.dev",
  vite: {
    plugins: [tailwindcss()],
  },
});
