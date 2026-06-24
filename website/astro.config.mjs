// @ts-check
import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";

// Static, zero-backend marketing site. `site` is used for absolute OG/canonical
// URLs (deployed to Cloudflare Workers static assets, custom domain autosk.app).
export default defineConfig({
  site: "https://autosk.app",
  vite: {
    plugins: [tailwindcss()],
  },
});
