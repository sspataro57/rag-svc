import type { ParsedFilters } from "./types";

// Recognized filter keys. Values that don't look like a date fall back to
// free text for `after:` so a typo doesn't silently drop the token.
const KEYS = new Set(["project", "space", "source", "after"]);

// Match a token of the form `key:value` anchored to word boundaries.
// Values cannot contain whitespace (quoted values aren't supported in v1 —
// no user has asked for them).
const TOKEN_RE = /(^|\s)(project|space|source|after):(\S+)/gi;

// YYYY-MM-DD or full RFC 3339 timestamp. We keep it conservative; the
// server accepts RFC 3339 exactly.
const DATE_RE = /^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2}))?$/;

/**
 * parseQuery splits a single input string into free-text + filter values.
 *
 * Examples:
 *   "credential rotation project:PLAT"
 *     → { text: "credential rotation", projects: ["PLAT"], ... }
 *   "project:PLAT project:OPS source:jira after:2026-01-01 runbook"
 *     → { text: "runbook", projects: ["PLAT", "OPS"], sources: ["jira"],
 *          updated_after: "2026-01-01" }
 *   "after:yesterday meeting notes"
 *     → { text: "after:yesterday meeting notes" }   // invalid date kept as text
 */
export function parseQuery(input: string): ParsedFilters {
  const out: ParsedFilters = {
    text: "",
    sources: [],
    projects: [],
    spaces: [],
  };

  const consumed = new Set<string>();
  for (const m of input.matchAll(TOKEN_RE)) {
    const key = m[2].toLowerCase();
    const value = m[3];
    if (!KEYS.has(key)) continue;
    if (key === "after") {
      if (!DATE_RE.test(value)) continue; // leave as free text
      out.updated_after = value;
    } else if (key === "project") {
      out.projects.push(value);
    } else if (key === "space") {
      out.spaces.push(value);
    } else if (key === "source") {
      out.sources.push(value);
    }
    consumed.add(m[0].trim());
  }

  // Build the free-text string by removing every token we consumed.
  let remaining = input;
  for (const token of consumed) {
    remaining = remaining.replace(token, " ");
  }
  out.text = remaining.replace(/\s+/g, " ").trim();
  return out;
}

/**
 * toSearchParams maps parsed filters onto the exact query-param shape the
 * Go /search handler expects.
 */
export function toSearchParams(f: ParsedFilters, limit?: number): URLSearchParams {
  const p = new URLSearchParams();
  p.set("q", f.text);
  if (limit && limit > 0) p.set("limit", String(limit));
  for (const s of f.sources) p.append("source", s);
  for (const pr of f.projects) p.append("project", pr);
  for (const sp of f.spaces) p.append("space", sp);
  if (f.updated_after) {
    // Backend wants RFC 3339; promote a bare date to start-of-day UTC.
    const v = /^\d{4}-\d{2}-\d{2}$/.test(f.updated_after)
      ? `${f.updated_after}T00:00:00Z`
      : f.updated_after;
    p.set("updated_after", v);
  }
  return p;
}
