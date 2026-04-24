import { defineManifest } from "@crxjs/vite-plugin";

// The `key` field pins the extension ID across unpacked reloads. Derived
// from a 2048-bit RSA keypair; the matching private key is intentionally
// not committed (unpacked installs don't need it, and the Web Store would
// re-sign anyway). Extension ID:
//   mcgmonphpfgfkhpjmmcgcelgcmpjcmmc
// Set rag-svc's `EXTENSION_ID` env var to that value so CORS admits the
// extension's origin (`chrome-extension://mcgmonphpfgfkhpjmmcgcelgcmpjcmmc`).
const PINNED_KEY =
  "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA5/sZblrhTEUxcjLxL/HlIukfXEKC1nZ6OJY0vIuIdCT6bNp0nRCGkBNMw57omZTN009Td68Z5RLntFQA1gfwCmWatoYcqf2nJ1SbAZDS4jZyurRJNFI+9mnOMR74zuBWZgF7r4lbdE/eRgg0DhrLE5c/xJfcI9nuMWwgt19OQfFHbT0WCqUzPfO8q01vZEJliGhUTTh7i6zZlVIYzYy10644R+e/sO8by9S6k4jpgu1euoAorPcRF4lNqVxuOQesV5/B3fa49BQesOZPyMQJR1mhGeP5XPoZl6FliymS7o8QEzvYuP+07Holl/EOi7RwkaA6eqJNae7yKkr+ZfWZ8wIDAQAB";

export default defineManifest({
  manifest_version: 3,
  name: "rag-svc supersearch",
  version: "0.1.0",
  description:
    "Cmd-K search across Treetop's Jira, Confluence, and indexed documents.",
  key: PINNED_KEY,
  permissions: ["storage"],
  host_permissions: ["https://treetopllc.jira.com/*"],
  // Lets the user grant broader hosts from the options page without
  // shipping a manifest change. Not required for v1 Treetop dogfooding.
  optional_host_permissions: ["https://*/*"],
  background: {
    service_worker: "src/background.ts",
    type: "module",
  },
  content_scripts: [
    {
      matches: ["https://treetopllc.jira.com/*"],
      js: ["src/content.ts"],
      run_at: "document_idle",
    },
  ],
  action: { default_title: "rag-svc supersearch" },
  options_page: "public/options.html",
  icons: {
    "16": "public/icon-16.png",
    "48": "public/icon-48.png",
    "128": "public/icon-128.png",
  },
});
