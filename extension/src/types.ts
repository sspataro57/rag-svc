// Shared wire-shape types for /search. Kept in sync by hand with the Go
// structs in `internal/http/search.go`; the test suite's `filters.test.ts`
// acts as a tripwire that fails if field names drift.

export interface SearchHit {
  id: string;
  source: string;
  title: string;
  snippet: string;
  url: string;
  project_or_space?: string;
  updated_at: string; // RFC 3339
  score: number;
  extra?: Record<string, unknown>;
}

export interface SearchMeta {
  limit: number;
  elapsed_ms: number;
  ticket_shortcut?: string;
}

export interface SearchResponse {
  query: string;
  hits: SearchHit[];
  meta: SearchMeta;
}

export interface SearchErrorResponse {
  error: string;
  retry_after_ms?: number;
}

// Parsed filter set the content script assembles before issuing the fetch.
// Keys mirror the /search query-param contract.
export interface ParsedFilters {
  text: string;
  sources: string[];
  projects: string[];
  spaces: string[];
  updated_after?: string; // ISO date at day granularity is fine for v1
}

// Message envelope between content script and service worker.
export type BackgroundRequest =
  | { kind: "search"; query: string; filters: ParsedFilters; limit?: number }
  | { kind: "ping" };

export type BackgroundResponse =
  | { ok: true; data: SearchResponse }
  | { ok: false; kind: "unauthenticated"; signInUrl: string }
  | { ok: false; kind: "rate_limited"; retryAfterMs: number }
  | { ok: false; kind: "misconfigured"; message: string }
  | { ok: false; kind: "network"; message: string }
  | { ok: false; kind: "server"; status: number; message: string };
