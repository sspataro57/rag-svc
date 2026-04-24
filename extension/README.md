# rag-svc supersearch (Chrome extension)

Cmd-K overlay that searches Treetop's Jira / Confluence / indexed documents
through [rag-svc](../README.md)'s `/search` endpoint. Manifest V3, plain
TypeScript in a closed Shadow DOM.

## Status

Rollout step 5 of the parent project. Scope:

- Manifest V3, pinned extension ID.
- Content script on `https://treetopllc.jira.com/*` with Cmd-K / Ctrl-K trigger.
- Closed-Shadow-DOM overlay with debounced search (150ms, in-flight aborts).
- Filter parser: `project:PLAT`, `space:ENG`, `source:jira`, `after:2026-01-01`.
- Keyboard nav: ↑/↓ selects, Enter opens, Cmd/Ctrl+Enter new tab, Esc dismisses.
- Session auth via rag-svc's `rag_svc_session` cookie; 401 shows a sign-in CTA.
- Options page to set the backend URL.

Out of scope for step 5: "Ask AI" fall-through (step 11), telemetry, Web
Store submission.

## Pinned extension ID

Manifest includes a `key` that pins the extension ID across unpacked
reloads so rag-svc's `EXTENSION_ID` CORS allowlist stays stable:

```
mcgmonphpfgfkhpjmmcgcelgcmpjcmmc
```

Set it once on the backend:

```bash
export EXTENSION_ID=mcgmonphpfgfkhpjmmcgcelgcmpjcmmc
docker compose -f ../deploy/compose/docker-compose.yml up -d --force-recreate rag-svc
```

## Build

```bash
npm install
npm run build          # → dist/
npm test               # vitest unit tests (filter parser + URL mapping)
npm run typecheck      # tsc --noEmit
```

`dist/` is what Chrome loads.

## Install (unpacked)

1. `npm install && npm run build`.
2. Open `chrome://extensions`, toggle **Developer mode** on.
3. Click **Load unpacked** → select this repo's `extension/dist` directory.
4. The extension appears with ID `mcgmonphpfgfkhpjmmcgcelgcmpjcmmc`. Pin
   it to the toolbar if you want.
5. Click the extension's **Details → Extension options** and enter the
   backend URL (e.g., `http://localhost:8080` for local dev, or
   `https://rag.treetopllc.com` for prod).

## Auth for dev

rag-svc in stub mode (no `OIDC_ISSUER` set) exposes `/dev/login`:

```
http://localhost:8080/dev/login?email=you@treetopllc.com
```

Visit it once in your browser; the session cookie is set for 7 days. The
extension's service-worker fetch includes the cookie automatically via
`credentials: "include"`.

## Manual verification checklist

Step 5 ships without automated browser tests — verify by hand after
`npm run build` and loading unpacked:

- [ ] Open `https://treetopllc.jira.com` (or any allowed host).
- [ ] Press **Cmd-K** (Mac) / **Ctrl-K** (Win/Linux). Overlay appears.
- [ ] Type a ticket key like `SANDBOX-5` — top hit shows score 1.0.
- [ ] Type keywords like `user story help` — multiple hits ordered by
      relevance, `<mark>` highlights in snippet.
- [ ] Type `project:SANDBOX bug` — hits narrowed to SANDBOX project.
- [ ] Try `after:2026-01-01 runbook` — date filter applied server-side.
- [ ] ↑/↓ moves selection, Enter opens in current tab, Cmd/Ctrl-Enter
      opens in new tab, Esc dismisses.
- [ ] Click outside the panel — dismisses.
- [ ] Clear the rag-svc session cookie (devtools → Application → Cookies)
      or visit with a fresh profile — Cmd-K shows the "Sign in" CTA.
- [ ] Rate-limit: set `SEARCH_RATE_LIMIT_PER_USER=3` on the backend,
      type rapidly several times — overlay shows "Rate limited. Try
      again in Ns."

## Layout

```
extension/
├── src/
│   ├── manifest.ts           # declarative manifest for @crxjs/vite-plugin
│   ├── background.ts         # service worker; owns the fetch
│   ├── content.ts            # injected content script; Cmd-K trigger
│   ├── overlay.ts            # closed Shadow DOM overlay, rendering, keyboard
│   ├── filters.ts            # parse project:/space:/source:/after: tokens
│   ├── storage.ts            # chrome.storage.sync wrapper
│   ├── types.ts              # wire shapes mirroring Go /search response
│   └── options/options.ts    # options-page logic
├── public/
│   ├── options.html          # options page shell
│   └── icon-{16,48,128}.png  # placeholder glyphs
├── test/filters.test.ts      # vitest
├── .manifest-key             # DER+base64 public key used in manifest.key
├── vite.config.ts
├── tsconfig.json
└── package.json
```

Private key (`key.pem`) lives on the author's machine only and is
`.gitignore`'d — unpacked installs derive the ID from the public key in
the manifest, and the Web Store would re-sign for any listed release.
