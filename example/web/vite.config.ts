import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

// The Go app embeds the build from dist/app, next to a placeholder that lets it compile before the first build
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: "dist/app",
    emptyOutDir: true,
  },
});
