import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { writeFileSync } from "node:fs";
import path from "node:path";

// frontend/dist/.gitkeep is committed so `go build` works on a fresh clone
// (frontend/embed.go embeds dist). Vite empties dist on every build, so put
// the placeholder back afterwards to keep the working tree clean.
function keepDistPlaceholder(): Plugin {
  return {
    name: "keep-dist-placeholder",
    apply: "build",
    closeBundle() {
      writeFileSync(path.resolve(import.meta.dirname, "dist/.gitkeep"), "");
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), keepDistPlaceholder()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    proxy: {
      "/login": "http://localhost:8000",
      "/status": "http://localhost:8000",
      "/account": "http://localhost:8000",
      "/build": "http://localhost:8000",
      "/docker": "http://localhost:8000",
      "/massrun": "http://localhost:8000",
      "/repo": "http://localhost:8000",
    },
  },
});
