// Content script: lives on treetopllc.jira.com pages, listens for Cmd-K,
// owns the overlay, debounces input, forwards searches to the service
// worker. No direct network calls — all HTTP goes through background.ts
// so the request origin is `chrome-extension://<id>`.

import { createOverlay } from "./overlay";
import { parseQuery } from "./filters";
import type { BackgroundRequest, BackgroundResponse, SearchHit } from "./types";

const DEBOUNCE_MS = 150;
const DEFAULT_LIMIT = 10;

const overlay = createOverlay({
  onInput: (value) => scheduleSearch(value),
  onDismiss: () => overlay.hide(),
  onOpen: (hit, newTab) => openHit(hit, newTab),
});

let debounceTimer: number | undefined;
let inflight: AbortController | undefined;

function scheduleSearch(value: string) {
  if (debounceTimer !== undefined) window.clearTimeout(debounceTimer);
  if (inflight) {
    inflight.abort();
    inflight = undefined;
  }
  if (!value.trim()) {
    overlay.setStatus("Type to search.");
    return;
  }
  debounceTimer = window.setTimeout(() => void runSearch(value), DEBOUNCE_MS);
}

async function runSearch(value: string) {
  overlay.setStatus("Searching…");
  const filters = parseQuery(value);
  inflight = new AbortController();
  // chrome.runtime.sendMessage doesn't support AbortController natively;
  // we use it as a logical cancel gate so a late response is discarded.
  const signal = inflight.signal;

  const msg: BackgroundRequest = {
    kind: "search",
    query: value,
    filters,
    limit: DEFAULT_LIMIT,
  };
  let resp: BackgroundResponse;
  try {
    resp = await chrome.runtime.sendMessage<BackgroundRequest, BackgroundResponse>(msg);
  } catch (e: unknown) {
    if (signal.aborted) return;
    overlay.setStatus(
      e instanceof Error ? e.message : "Lost the service worker connection.",
      "error"
    );
    return;
  }
  if (signal.aborted) return;

  if (resp.ok) {
    overlay.setResults(resp.data.hits);
    return;
  }
  switch (resp.kind) {
    case "unauthenticated":
      overlay.setSignInPrompt(resp.signInUrl);
      break;
    case "rate_limited":
      overlay.setStatus(
        `Rate limited. Try again in ${Math.max(1, Math.round(resp.retryAfterMs / 1000))}s.`,
        "error"
      );
      break;
    case "misconfigured":
      overlay.setStatus(resp.message, "error");
      break;
    case "network":
      overlay.setStatus(`Network error: ${resp.message}`, "error");
      break;
    case "server":
      overlay.setStatus(`Server error ${resp.status}: ${resp.message}`, "error");
      break;
  }
}

function openHit(hit: SearchHit, newTab: boolean) {
  if (!hit.url) return;
  if (newTab) {
    window.open(hit.url, "_blank", "noopener");
  } else {
    window.location.href = hit.url;
  }
  overlay.hide();
}

// Capturing-phase listener so we preempt Jira's own shortcut handlers.
window.addEventListener(
  "keydown",
  (ev) => {
    // Ignore typing inside inputs/textareas unless the overlay is already
    // open (Esc inside the overlay input still needs to dismiss).
    const inField = isEditable(ev.target);
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
      ev.preventDefault();
      ev.stopPropagation();
      if (overlay.isVisible()) {
        overlay.hide();
      } else {
        overlay.show();
      }
      return;
    }
    if (ev.key === "Escape" && overlay.isVisible() && !inField) {
      // Overlay's own listener handles Esc when the input has focus; this
      // catches Esc when focus is elsewhere.
      overlay.hide();
    }
  },
  true
);

function isEditable(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  const tag = t.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA") return true;
  if (t.isContentEditable) return true;
  return false;
}
