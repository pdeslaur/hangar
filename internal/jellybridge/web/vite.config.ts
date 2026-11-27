import { defineConfig } from "vite";
import solid from "@solidjs/vite-plugin";

// The pages are served under /sso/, behind the sign-on, and their files
// under /sso-static/, outside it; public/ as is: bridge.js keeps its name,
// since the proxy adds it to jellyfin-web's page.
export default defineConfig({
  base: "/sso-static/",
  plugins: [solid()],
  build: {
    rolldownOptions: {
      input: ["account.html", "login.html"],
    },
  },
});
