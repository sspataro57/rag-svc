import { defineConfig } from "vite";
import { crx } from "@crxjs/vite-plugin";
import manifest from "./src/manifest";

export default defineConfig({
  plugins: [crx({ manifest })],
  build: {
    // Extension bundle is tiny and self-contained; skip sourcemaps to keep
    // release zips compact. Flip to true when debugging.
    sourcemap: false,
    rollupOptions: {
      input: {
        options: "public/options.html",
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["test/**/*.test.ts"],
  },
});
