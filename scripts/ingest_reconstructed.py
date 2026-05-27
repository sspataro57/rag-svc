#!/usr/bin/env python3
"""Feed reconstructed Jira tickets into rag-svc via POST /ingest/jira.

Source data shape (from /home/salvo/Downloads/emails/jira_tickets.json):

    [
      {"number": "TES-1",
       "title": "...",
       "description": "",
       "status": "In Requirement Review Approved",
       "assignee": "Salvador Spataro",
       "reporter": "Salvador Spataro",
       "created_at": "2017-12-30T00:02:13+00:00",
       "updated_at": "2019-02-11T10:00:37+00:00",
       "email_count": 20,
       "comments": ["...", "..."]},
      ...
    ]

Mapping decisions (see the rag-svc CLAUDE.md and project_two_instance_deploy
memory):

* `number` is the Jira-style key. Project derives from its prefix.
* Comments are bare strings — author and timestamp are unknown. We attribute
  them to a configurable display name (default "Salvador Spataro") because
  empty-author comments in the reconstructed corpus were almost all written
  by Salvador. The rag-svc chunker will skip the "by ... on ..." header
  entirely when both author and timestamp are empty, so passing the author
  here is what gets it into the embedding context.
* Project allow-list defaults to NW + TES (the two real bodies of work);
  NS and TEST are excluded as test projects.
* updated_at is required by the endpoint and always present in the source.

Usage:
    RAG_INGEST_URL=https://... RAG_INGEST_TOKEN=...     ./scripts/ingest_reconstructed.py /path/to/jira_tickets.json

By default the script runs in dry-run mode and prints batch summaries
without POSTing — pass --apply to actually send.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from collections import Counter
from typing import Any, Iterable
from urllib import error as urlerror
from urllib import request as urlrequest

DEFAULT_PROJECTS = {"NW", "TES"}
DEFAULT_BATCH_SIZE = 100
DEFAULT_AUTHOR = "Salvador Spataro"
# rag-svc caps batches at 200 (see internal/http/ingest.go:maxIngestBatchSize).
MAX_BATCH_SIZE = 200


def project_of(key: str) -> str:
    return key.split("-", 1)[0] if "-" in key else key


def ticket_to_payload(t: dict[str, Any], default_author: str) -> dict[str, Any]:
    """Convert a reconstructed-ticket dict to the /ingest/jira wire shape."""
    key = t["number"]
    comments_in = t.get("comments") or []
    comments_out = []
    for body in comments_in:
        if not isinstance(body, str):
            continue
        body = body.strip()
        if not body:
            continue
        comments_out.append({
            "author": default_author,
            # No created_at — endpoint accepts zero/missing timestamps
            # and the chunker emits a bare "## Comment" divider in that
            # case (see internal/chunk/chunk.go:commentHeader).
            "body": body,
        })

    extra: dict[str, Any] = {"reconstructed": True}
    if t.get("email_count"):
        extra["email_count"] = t["email_count"]
    if t.get("assignee"):
        extra["assignee"] = t["assignee"]
    if t.get("reporter"):
        extra["reporter"] = t["reporter"]

    return {
        "key": key,
        "project": project_of(key),
        "title": t.get("title") or "",
        "description": t.get("description") or "",
        "status": t.get("status") or "",
        # url stays empty — these tickets have no live source URL.
        "url": "",
        "updated_at": t["updated_at"],
        "comments": comments_out,
        "extra": extra,
    }


def batched(items: list, n: int) -> Iterable[list]:
    for i in range(0, len(items), n):
        yield items[i:i + n]


def post(url: str, token: str, payload: dict[str, Any]) -> dict[str, Any]:
    body = json.dumps(payload).encode("utf-8")
    req = urlrequest.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {token}",
        },
    )
    try:
        with urlrequest.urlopen(req, timeout=300) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urlerror.HTTPError as e:
        # Read the response body for context — handler returns JSON on errors too.
        detail = e.read().decode("utf-8", errors="replace")
        raise SystemExit(f"POST {url} -> {e.code}: {detail}") from None


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("path", help="Path to jira_tickets.json")
    p.add_argument("--projects", default=",".join(sorted(DEFAULT_PROJECTS)),
                   help="Comma-separated project allow-list (empty = all). Default: %(default)s")
    p.add_argument("--author", default=DEFAULT_AUTHOR,
                   help="Default author for comments with no recorded author. Default: %(default)s")
    p.add_argument("--batch-size", type=int, default=DEFAULT_BATCH_SIZE,
                   help=f"Tickets per request (max {MAX_BATCH_SIZE}). Default: %(default)s")
    p.add_argument("--limit", type=int, default=0,
                   help="Stop after sending this many tickets (0 = all)")
    p.add_argument("--apply", action="store_true",
                   help="Actually POST. Without this the script is a dry run.")
    p.add_argument("--url", default=os.environ.get("RAG_INGEST_URL", ""),
                   help="Override RAG_INGEST_URL")
    p.add_argument("--token", default=os.environ.get("RAG_INGEST_TOKEN", ""),
                   help="Override RAG_INGEST_TOKEN")
    args = p.parse_args()

    if args.batch_size < 1 or args.batch_size > MAX_BATCH_SIZE:
        print(f"batch-size must be 1..{MAX_BATCH_SIZE}", file=sys.stderr)
        return 2

    allow = {s.strip() for s in args.projects.split(",") if s.strip()}

    with open(args.path, "r", encoding="utf-8") as fh:
        tickets = json.load(fh)
    if not isinstance(tickets, list):
        print(f"{args.path}: expected a JSON array, got {type(tickets).__name__}", file=sys.stderr)
        return 2

    by_project = Counter(project_of(t["number"]) for t in tickets if "number" in t)
    print(f"loaded {len(tickets)} tickets across {len(by_project)} projects: {dict(by_project)}")

    if allow:
        kept = [t for t in tickets if "number" in t and project_of(t["number"]) in allow]
        print(f"after project filter {sorted(allow)}: {len(kept)} tickets")
    else:
        kept = [t for t in tickets if "number" in t]
        print("no project filter; all projects included")

    if args.limit and len(kept) > args.limit:
        kept = kept[: args.limit]
        print(f"--limit applied: {len(kept)} tickets")

    payloads = [ticket_to_payload(t, args.author) for t in kept]

    if not args.apply:
        sample = payloads[0] if payloads else {}
        print("DRY RUN — no requests will be made. Sample payload:")
        print(json.dumps(sample, indent=2)[:1500])
        print(f"would send {len(payloads)} tickets in {((len(payloads) + args.batch_size - 1) // args.batch_size)} batches of up to {args.batch_size}")
        return 0

    if not args.url or not args.token:
        print("--apply requires --url/RAG_INGEST_URL and --token/RAG_INGEST_TOKEN", file=sys.stderr)
        return 2

    endpoint = args.url.rstrip("/") + "/ingest/jira"
    sent, upserted, chunks, failed = 0, 0, 0, 0
    started = time.time()

    for i, batch in enumerate(batched(payloads, args.batch_size), start=1):
        body = {"issues": batch}
        t0 = time.time()
        resp = post(endpoint, args.token, body)
        elapsed = time.time() - t0
        b_up = resp.get("upserted", 0)
        b_ch = resp.get("chunks", 0)
        b_fa = resp.get("failed", 0)
        sent += len(batch)
        upserted += b_up
        chunks += b_ch
        failed += b_fa
        print(f"batch {i}: {len(batch)} sent | upserted={b_up} chunks={b_ch} failed={b_fa} ({elapsed:.1f}s)")
        for err in resp.get("errors", [])[:5]:
            print(f"  ! {err.get('key')}: {err.get('error')}")

    total = time.time() - started
    print(f"done: {sent} sent | upserted={upserted} chunks={chunks} failed={failed} in {total:.1f}s")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
