// Closed-Shadow-DOM overlay. Exposed as a factory: createOverlay() returns
// an object the content script owns, with imperative `show`/`hide`/`render`
// methods. Keeping the DOM inside a closed Shadow DOM means Jira's CSS
// can't bleed in and the overlay's styles can't bleed out.

import type { SearchHit } from "./types";

const STYLE = `
  :host { all: initial; }
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(15, 23, 42, 0.35);
    display: flex; align-items: flex-start; justify-content: center;
    padding-top: 12vh;
    z-index: 2147483647;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  .panel {
    width: 640px; max-width: calc(100vw - 40px);
    max-height: 70vh; overflow: hidden;
    background: white; border-radius: 12px;
    box-shadow: 0 20px 60px rgba(0, 0, 0, 0.25);
    display: flex; flex-direction: column;
  }
  .input {
    appearance: none; border: none; outline: none;
    padding: 18px 20px; font-size: 16px;
    border-bottom: 1px solid #e2e8f0;
    background: white; color: #0f172a;
  }
  .status {
    padding: 14px 20px; font-size: 13px; color: #64748b;
  }
  .status.error { color: #b91c1c; }
  .results { overflow-y: auto; }
  .hit {
    padding: 12px 20px; border-bottom: 1px solid #f1f5f9;
    cursor: pointer; display: block;
  }
  .hit.selected, .hit:hover { background: #f1f5f9; }
  .hit-title { font-size: 14px; color: #0f172a; font-weight: 600; margin-bottom: 4px; }
  .hit-snippet { font-size: 13px; color: #334155; line-height: 1.4; margin-bottom: 4px; }
  .hit-snippet mark { background: #fef08a; color: #854d0e; padding: 0 2px; }
  .hit-meta { font-size: 11px; color: #64748b; text-transform: uppercase; letter-spacing: 0.05em; }
  .hit-meta span + span::before { content: "  ·  "; color: #cbd5e1; }
  .signin { padding: 20px; text-align: center; }
  .signin a {
    display: inline-block; margin-top: 6px;
    color: #0f766e; text-decoration: underline;
  }
`;

export interface OverlayCallbacks {
  onInput(value: string): void;
  onDismiss(): void;
  onOpen(hit: SearchHit, newTab: boolean): void;
}

export interface OverlayView {
  show(): void;
  hide(): void;
  isVisible(): boolean;
  setStatus(text: string, kind?: "info" | "error"): void;
  setResults(hits: SearchHit[]): void;
  setSignInPrompt(signInUrl: string): void;
  destroy(): void;
}

export function createOverlay(cb: OverlayCallbacks): OverlayView {
  const host = document.createElement("div");
  host.id = "rag-svc-overlay-host";
  // Non-interfering host element — size 0, no layout.
  host.style.cssText = "all: initial; position: fixed; width: 0; height: 0;";
  const shadow = host.attachShadow({ mode: "closed" });

  const styleEl = document.createElement("style");
  styleEl.textContent = STYLE;
  shadow.appendChild(styleEl);

  const backdrop = document.createElement("div");
  backdrop.className = "backdrop";
  backdrop.style.display = "none";

  const panel = document.createElement("div");
  panel.className = "panel";

  const input = document.createElement("input");
  input.className = "input";
  input.type = "text";
  input.placeholder = "Search Treetop (Cmd-K)…  try project:PLAT or after:2026-01-01";
  input.autocomplete = "off";

  const status = document.createElement("div");
  status.className = "status";
  status.textContent = "";

  const results = document.createElement("div");
  results.className = "results";

  panel.appendChild(input);
  panel.appendChild(status);
  panel.appendChild(results);
  backdrop.appendChild(panel);
  shadow.appendChild(backdrop);
  document.documentElement.appendChild(host);

  let selected = 0;
  let currentHits: SearchHit[] = [];

  const render = (hits: SearchHit[]) => {
    currentHits = hits;
    results.innerHTML = "";
    hits.forEach((h, i) => {
      const row = document.createElement("div");
      row.className = "hit" + (i === selected ? " selected" : "");
      row.dataset.idx = String(i);

      const title = document.createElement("div");
      title.className = "hit-title";
      title.textContent = h.title || h.id;
      row.appendChild(title);

      // Snippet may contain <mark> tags from ts_headline. We trust rag-svc's
      // output (HTML-escaped aside from <mark>) but strip anything else to
      // reduce XSS risk if a future server bug leaks raw content.
      const snippet = document.createElement("div");
      snippet.className = "hit-snippet";
      snippet.innerHTML = sanitizeSnippet(h.snippet);
      row.appendChild(snippet);

      const meta = document.createElement("div");
      meta.className = "hit-meta";
      const bits: string[] = [h.source];
      if (h.project_or_space) bits.push(h.project_or_space);
      if (h.updated_at) bits.push(relativeTime(h.updated_at));
      if (h.extra && typeof h.extra === "object") {
        const extra = h.extra as Record<string, unknown>;
        if (typeof extra.status === "string" && extra.status) bits.push(String(extra.status));
        if (typeof extra.issue_type === "string" && extra.issue_type) bits.push(String(extra.issue_type));
      }
      for (const b of bits) {
        const s = document.createElement("span");
        s.textContent = b;
        meta.appendChild(s);
      }
      row.appendChild(meta);

      row.addEventListener("click", (ev) => {
        cb.onOpen(h, ev.metaKey || ev.ctrlKey);
      });
      results.appendChild(row);
    });
  };

  const updateSelection = () => {
    const rows = results.querySelectorAll<HTMLDivElement>(".hit");
    rows.forEach((row, i) => {
      row.classList.toggle("selected", i === selected);
      if (i === selected) row.scrollIntoView({ block: "nearest" });
    });
  };

  const onKeyDown = (ev: KeyboardEvent) => {
    if (backdrop.style.display === "none") return;
    if (ev.key === "Escape") {
      cb.onDismiss();
      return;
    }
    if (!currentHits.length) return;
    if (ev.key === "ArrowDown") {
      ev.preventDefault();
      selected = Math.min(selected + 1, currentHits.length - 1);
      updateSelection();
    } else if (ev.key === "ArrowUp") {
      ev.preventDefault();
      selected = Math.max(selected - 1, 0);
      updateSelection();
    } else if (ev.key === "Enter") {
      ev.preventDefault();
      const hit = currentHits[selected];
      if (hit) cb.onOpen(hit, ev.metaKey || ev.ctrlKey);
    }
  };

  input.addEventListener("input", () => {
    selected = 0;
    cb.onInput(input.value);
  });
  backdrop.addEventListener("click", (ev) => {
    if (ev.target === backdrop) cb.onDismiss();
  });
  // Listen on the host element so the overlay's own keyboard events don't
  // escape to the page (Jira also binds shortcuts).
  shadow.addEventListener("keydown", onKeyDown as EventListener);

  return {
    show() {
      backdrop.style.display = "flex";
      // Focus the input next tick so Chrome doesn't eat the keystroke.
      setTimeout(() => input.focus(), 0);
    },
    hide() {
      backdrop.style.display = "none";
      input.value = "";
      status.textContent = "";
      status.className = "status";
      results.innerHTML = "";
      currentHits = [];
      selected = 0;
    },
    isVisible() {
      return backdrop.style.display !== "none";
    },
    setStatus(text, kind) {
      status.className = "status" + (kind === "error" ? " error" : "");
      status.textContent = text;
      results.innerHTML = "";
      currentHits = [];
    },
    setResults(hits) {
      status.textContent = hits.length === 0 ? "No results." : "";
      status.className = "status";
      selected = 0;
      render(hits);
    },
    setSignInPrompt(signInUrl) {
      status.textContent = "";
      results.innerHTML = "";
      const wrap = document.createElement("div");
      wrap.className = "signin";
      const msg = document.createElement("div");
      msg.textContent = "You're not signed in to rag-svc.";
      const link = document.createElement("a");
      link.href = signInUrl;
      link.target = "_blank";
      link.rel = "noopener";
      link.textContent = "Sign in to continue →";
      wrap.appendChild(msg);
      wrap.appendChild(link);
      results.appendChild(wrap);
    },
    destroy() {
      host.remove();
    },
  };
}

// sanitizeSnippet allows only <mark>…</mark>. Everything else is
// text-escaped. Defense in depth — rag-svc's ts_headline output is already
// trustworthy, but the content script runs in Jira's page context and
// anything we inject becomes part of the DOM, so we're conservative.
export function sanitizeSnippet(s: string): string {
  const escaped = s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return escaped
    .replace(/&lt;mark&gt;/g, "<mark>")
    .replace(/&lt;\/mark&gt;/g, "</mark>");
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const diffMs = Date.now() - t;
  const m = Math.floor(diffMs / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d}d ago`;
  const mo = Math.floor(d / 30);
  if (mo < 12) return `${mo}mo ago`;
  return `${Math.floor(mo / 12)}y ago`;
}
