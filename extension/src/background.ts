// Service worker. The only job in step 5 is relaying `/search` requests
// from the content script through a `credentials: "include"` fetch — doing
// it here means the request origin is `chrome-extension://<EXTENSION_ID>`,
// which rag-svc's CORS middleware allowlists. Content-script-side fetches
// would carry the page origin (treetopllc.jira.com) instead and would need
// that host allowlisted too.

import { toSearchParams } from "./filters";
import { getSettings } from "./storage";
import type { BackgroundRequest, BackgroundResponse, SearchResponse } from "./types";

chrome.runtime.onMessage.addListener(
  (msg: BackgroundRequest, _sender, sendResponse) => {
    // sendResponse must be called asynchronously — return `true` to keep
    // the channel open per the extension API contract.
    handle(msg)
      .then(sendResponse)
      .catch((err) =>
        sendResponse({
          ok: false,
          kind: "network",
          message: err?.message ?? String(err),
        } satisfies BackgroundResponse)
      );
    return true;
  }
);

async function handle(msg: BackgroundRequest): Promise<BackgroundResponse> {
  if (msg.kind === "ping") {
    return { ok: true, data: { query: "", hits: [], meta: { limit: 0, elapsed_ms: 0 } } };
  }
  if (msg.kind !== "search") {
    return { ok: false, kind: "server", status: 0, message: "unknown request kind" };
  }

  const settings = await getSettings();
  if (!settings.backendUrl) {
    return {
      ok: false,
      kind: "misconfigured",
      message:
        "Set backend URL in the extension options first (chrome://extensions → rag-svc → Details → Extension options).",
    };
  }

  const params = toSearchParams(msg.filters, msg.limit);
  // parseQuery strips filter tokens from `text`; favor that if present.
  if (msg.filters.text) params.set("q", msg.filters.text);
  else params.set("q", msg.query);

  const url = joinURL(settings.backendUrl, "/search") + "?" + params.toString();

  let resp: Response;
  try {
    resp = await fetch(url, {
      method: "GET",
      credentials: "include",
      headers: { Accept: "application/json" },
    });
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : String(e);
    return { ok: false, kind: "network", message };
  }

  if (resp.status === 401) {
    return {
      ok: false,
      kind: "unauthenticated",
      signInUrl: joinURL(settings.backendUrl, "/dev/login"),
    };
  }
  if (resp.status === 429) {
    let retry = 0;
    try {
      const body = (await resp.json()) as { retry_after_ms?: number };
      retry = body.retry_after_ms ?? 0;
    } catch {
      // fall through
    }
    return { ok: false, kind: "rate_limited", retryAfterMs: retry };
  }
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    return {
      ok: false,
      kind: "server",
      status: resp.status,
      message: text.slice(0, 200),
    };
  }
  const data = (await resp.json()) as SearchResponse;
  return { ok: true, data };
}

// joinURL handles "backendUrl with trailing slash" + "path with leading slash"
// without producing "//".
function joinURL(base: string, path: string): string {
  const b = base.replace(/\/+$/, "");
  const p = path.replace(/^\/+/, "");
  return `${b}/${p}`;
}
