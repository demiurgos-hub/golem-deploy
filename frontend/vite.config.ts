import { defineConfig, loadEnv, type Plugin } from "vite";
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

// The dev-only API mock (src/mock) must never ship. main.tsx imports it only
// behind import.meta.env.DEV, which is false in builds; this fails the build
// if the module or its marker string still made it into any output chunk.
function assertNoMockInBundle(): Plugin {
  return {
    name: "assert-no-mock-in-bundle",
    apply: "build",
    generateBundle(_options, bundle) {
      for (const [file, chunk] of Object.entries(bundle)) {
        const code = chunk.type === "chunk" ? chunk.code : String(chunk.source);
        const fromMock = chunk.type === "chunk" && Object.keys(chunk.modules).some((id) => id.includes("/src/mock/"));
        if (fromMock || code.includes("golem-deploy-mock-layer")) {
          this.error(`the dev-only API mock ended up in ${file}`);
        }
      }
    },
  };
}

export default defineConfig(({ command, mode }) => {
  const env = loadEnv(mode, import.meta.dirname, "VITE_");
  if (command === "build" && mode !== "test" && env.VITE_API_MOCK) {
    throw new Error("VITE_API_MOCK is set: the API mock is dev-only and cannot be built into the web client.");
  }
  return {
    plugins: [react(), tailwindcss(), keepDistPlaceholder(), assertNoMockInBundle()],
    resolve: {
      alias: {
        "@": path.resolve(import.meta.dirname, "./src"),
      },
    },
    build: {
      // Every browser the client supports has native modulepreload.
      modulePreload: { polyfill: false },
    },
    server: {
      proxy: {
        // GOLEM_DEPLOY_DEV_API points the dev server at a daemon on another address.
        "/api": process.env.GOLEM_DEPLOY_DEV_API ?? "http://127.0.0.1:8000",
      },
    },
  };
});
