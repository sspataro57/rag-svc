import { describe, expect, it } from "vitest";
import { parseQuery, toSearchParams } from "../src/filters";

describe("parseQuery", () => {
  it("extracts free text with no filters", () => {
    const p = parseQuery("credential rotation");
    expect(p.text).toBe("credential rotation");
    expect(p.projects).toEqual([]);
    expect(p.sources).toEqual([]);
    expect(p.spaces).toEqual([]);
    expect(p.updated_after).toBeUndefined();
  });

  it("extracts a single project token", () => {
    const p = parseQuery("credential rotation project:PLAT");
    expect(p.text).toBe("credential rotation");
    expect(p.projects).toEqual(["PLAT"]);
  });

  it("extracts multiple projects, sources, spaces, after in one query", () => {
    const p = parseQuery(
      "project:PLAT project:OPS source:jira space:ENG after:2026-01-01 runbook story"
    );
    expect(p.projects).toEqual(["PLAT", "OPS"]);
    expect(p.sources).toEqual(["jira"]);
    expect(p.spaces).toEqual(["ENG"]);
    expect(p.updated_after).toBe("2026-01-01");
    expect(p.text).toBe("runbook story");
  });

  it("accepts full RFC 3339 after: value", () => {
    const p = parseQuery("after:2026-01-15T09:30:00Z runbook");
    expect(p.updated_after).toBe("2026-01-15T09:30:00Z");
    expect(p.text).toBe("runbook");
  });

  it("keeps invalid after: as free text", () => {
    const p = parseQuery("after:yesterday meeting notes");
    expect(p.updated_after).toBeUndefined();
    expect(p.text).toBe("after:yesterday meeting notes");
  });

  it("is case-insensitive on filter keys", () => {
    const p = parseQuery("Project:PLAT Source:JIRA foo");
    expect(p.projects).toEqual(["PLAT"]);
    expect(p.sources).toEqual(["JIRA"]);
    expect(p.text).toBe("foo");
  });

  it("ignores unknown filter keys", () => {
    const p = parseQuery("assignee:alice foo");
    expect(p.text).toBe("assignee:alice foo");
    expect(p.projects).toEqual([]);
  });

  it("handles empty input", () => {
    const p = parseQuery("");
    expect(p.text).toBe("");
    expect(p.projects).toEqual([]);
  });

  it("collapses extra whitespace after token removal", () => {
    const p = parseQuery("  project:PLAT   foo   bar  ");
    expect(p.text).toBe("foo bar");
  });
});

describe("toSearchParams", () => {
  it("builds the expected query string", () => {
    const p = parseQuery(
      "project:PLAT project:OPS source:jira after:2026-01-01 runbook"
    );
    const params = toSearchParams(p, 10);
    expect(params.get("q")).toBe("runbook");
    expect(params.get("limit")).toBe("10");
    expect(params.getAll("project")).toEqual(["PLAT", "OPS"]);
    expect(params.getAll("source")).toEqual(["jira"]);
    expect(params.get("updated_after")).toBe("2026-01-01T00:00:00Z");
  });

  it("passes RFC 3339 after through unchanged", () => {
    const p = parseQuery("after:2026-01-15T09:30:00Z foo");
    const params = toSearchParams(p);
    expect(params.get("updated_after")).toBe("2026-01-15T09:30:00Z");
  });

  it("omits limit when unset", () => {
    const p = parseQuery("foo");
    const params = toSearchParams(p);
    expect(params.has("limit")).toBe(false);
  });
});
