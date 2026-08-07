#!/usr/bin/env python3
"""
pr-review-metrics.py — measure PR review latency, issue traction, and CI flakiness.

Subcommands
───────────
  snapshot          PR review latency for a date window
  compare           Compare PR review latency across two windows
  issue-snapshot    Issue traction stats for a date window
  issue-compare     Compare issue traction across two windows
  ci-snapshot       CI flakiness stats for a date window
  ci-compare        Compare CI flakiness across two windows

PR metrics
  first_review_hours  — PR opened → first non-author review event (any state)
  re_review_hours     — CHANGES_REQUESTED → next review (re-review turnaround)

Issue metrics
  first_comment_hours — issue opened → first non-author, non-bot comment
  time_to_close_hours — issue opened → closed (closed issues only)
  comment_count       — distribution of external comments per issue

CI flakiness metrics
  pr_flakiness_rate   — % of multi-run PR branches that had both a failure and a
                        success on the same code (i.e. needed a re-run to go green)
  main_failure_rate   — % of push-to-main workflow runs that failed

Usage
─────
  # PR snapshot
  python hack/pr-review-metrics.py snapshot \\
    --repo agent-substrate/substrate \\
    --since 2026-07-01 --until 2026-07-31 \\
    --save pr-before.json

  # Issue snapshot
  python hack/pr-review-metrics.py issue-snapshot \\
    --repo agent-substrate/substrate \\
    --since 2026-07-01 --until 2026-07-31 \\
    --save issue-before.json

  # CI flakiness snapshot (fetch up to 1000 runs, filter to window)
  python hack/pr-review-metrics.py ci-snapshot \\
    --repo agent-substrate/substrate \\
    --since 2026-07-01 --until 2026-08-07 \\
    --limit 1000 \\
    --save ci-before.json

  # Compare two windows (live or from saved files)
  python hack/pr-review-metrics.py compare \\
    --repo agent-substrate/substrate \\
    --before-file pr-before.json --after 2026-09-01:2026-09-30

  python hack/pr-review-metrics.py issue-compare \\
    --repo agent-substrate/substrate \\
    --before-file issue-before.json --after 2026-09-01:2026-09-30

  python hack/pr-review-metrics.py ci-compare \\
    --repo agent-substrate/substrate \\
    --before-file ci-before.json --after 2026-09-01:2026-09-30
"""

from __future__ import annotations

import argparse
import csv
import io
import json
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timezone
from statistics import mean, median, quantiles
from typing import Optional


# ── GitHub data fetching ──────────────────────────────────────────────────────

def _gh(*args: str) -> list:
    cmd = ["gh"] + list(args)
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        print(f"gh error: {result.stderr.strip()}", file=sys.stderr)
        sys.exit(1)
    return json.loads(result.stdout)


def fetch_prs(repo: str, since: str, until: str, limit: int = 500) -> list[dict]:
    """Return closed PRs whose createdAt falls within [since, until] (inclusive)."""
    since_dt = _parse_date(since)
    until_dt = _parse_date(until, end_of_day=True)
    raw = _gh("pr", "list", "--repo", repo, "--state", "closed",
              "--limit", str(limit),
              "--json", "number,title,createdAt,closedAt,mergedAt,author,reviews")
    return [pr for pr in raw if since_dt <= _parse_dt(pr["createdAt"]) <= until_dt]


def fetch_issues(repo: str, since: str, until: str, limit: int = 500) -> list[dict]:
    """Return all issues (open + closed) whose createdAt falls within [since, until]."""
    since_dt = _parse_date(since)
    until_dt = _parse_date(until, end_of_day=True)
    raw = _gh("issue", "list", "--repo", repo, "--state", "all",
              "--limit", str(limit),
              "--json", "number,title,createdAt,closedAt,state,author,comments,stateReason")
    issues = [iss for iss in raw if since_dt <= _parse_dt(iss["createdAt"]) <= until_dt]
    _enrich_closer(repo, issues)
    return issues


def _enrich_closer(repo: str, issues: list[dict]) -> None:
    """
    For each closed issue with no external comments, query GraphQL in batches to
    find what closed it: a PullRequest, a Commit (usually from a PR merge), or
    nothing (manually closed via the GitHub UI).

    Adds a 'closer_type' key to each issue: 'PullRequest', 'Commit', 'manual', or None.
    """
    owner, name = repo.split("/")
    targets = [iss for iss in issues if iss.get("state") == "CLOSED"]

    BATCH = 20
    for i in range(0, len(targets), BATCH):
        batch = targets[i:i + BATCH]
        aliases = "\n".join(
            f'i{iss["number"]}: issue(number: {iss["number"]}) {{'
            f' timelineItems(itemTypes: [CLOSED_EVENT], last: 1) {{'
            f'  nodes {{ ... on ClosedEvent {{'
            f'   closer {{ __typename }} }} }} }} }}'
            for iss in batch
        )
        query = f'{{ repository(owner: "{owner}", name: "{name}") {{ {aliases} }} }}'
        try:
            result = subprocess.run(
                ["gh", "api", "graphql", "-f", f"query={query}"],
                capture_output=True, text=True,
            )
            if result.returncode != 0:
                continue
            data = json.loads(result.stdout).get("data", {}).get("repository", {})
        except Exception:
            continue

        number_to_issue = {iss["number"]: iss for iss in batch}
        for iss in batch:
            key = f"i{iss['number']}"
            nodes = data.get(key, {}).get("timelineItems", {}).get("nodes", [])
            node = nodes[0] if nodes else None
            typename = (node or {}).get("closer", {}) or {}
            typename = typename.get("__typename") if isinstance(typename, dict) else None
            iss["closer_type"] = typename  # 'PullRequest', 'Commit', or None


def fetch_ci_runs(repo: str, since: str, until: str, limit: int = 1000,
                  workflow: str = "pr-workflow") -> list[dict]:
    """
    Return pr-workflow runs whose createdAt falls within [since, until].
    Fetches up to `limit` most-recent runs then filters by date client-side.
    Increase --limit if your window is older than what the default covers.
    """
    since_dt = _parse_date(since)
    until_dt = _parse_date(until, end_of_day=True)
    raw = _gh("run", "list", "--repo", repo, "--workflow", workflow,
              "--limit", str(limit),
              "--json", "databaseId,conclusion,createdAt,headBranch,event")
    # keep only completed runs with a definitive conclusion
    keep = {"success", "failure"}
    return [r for r in raw
            if r["conclusion"] in keep
            and since_dt <= _parse_dt(r["createdAt"]) <= until_dt]


# ── time helpers ──────────────────────────────────────────────────────────────

def _parse_dt(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def _parse_date(s: str, end_of_day: bool = False) -> datetime:
    dt = datetime.fromisoformat(s).replace(tzinfo=timezone.utc)
    if end_of_day:
        dt = dt.replace(hour=23, minute=59, second=59)
    return dt


def _hours(delta) -> float:
    return delta.total_seconds() / 3600


def _fmt_hours(h: Optional[float]) -> str:
    if h is None:
        return "n/a"
    if h < 1:
        return f"{int(h * 60)}m"
    if h < 48:
        return f"{h:.1f}h"
    return f"{h / 24:.1f}d"


def _is_bot(login: str) -> bool:
    return login.endswith("[bot]") or login in {"github-actions", "dependabot", "codecov"}


# ── per-PR computation ────────────────────────────────────────────────────────

def compute_pr_stats(pr: dict) -> dict:
    author = pr["author"]["login"]
    created = _parse_dt(pr["createdAt"])
    closed_at = _parse_dt(pr["closedAt"]) if pr.get("closedAt") else None

    external = sorted(
        (r for r in pr["reviews"] if r["author"]["login"] != author),
        key=lambda r: r["submittedAt"],
    )

    first_review_hours: Optional[float] = None
    if external:
        first_review_hours = _hours(_parse_dt(external[0]["submittedAt"]) - created)

    re_review_hours: list[float] = []
    for i, review in enumerate(external):
        if review["state"] == "CHANGES_REQUESTED" and i + 1 < len(external):
            gap = _hours(
                _parse_dt(external[i + 1]["submittedAt"])
                - _parse_dt(review["submittedAt"])
            )
            re_review_hours.append(gap)

    return {
        "number": pr["number"],
        "title": pr["title"],
        "author": author,
        "created_at": pr["createdAt"],
        "merged_at": pr.get("mergedAt"),
        "closed_at": pr.get("closedAt"),
        "first_review_hours": first_review_hours,
        "re_review_hours": re_review_hours,
        "review_count": len(external),
        "total_hours_open": _hours(closed_at - created) if closed_at else None,
    }


# ── per-issue computation ─────────────────────────────────────────────────────

def compute_issue_stats(issue: dict) -> dict:
    author = issue["author"]["login"]
    created = _parse_dt(issue["createdAt"])
    closed_at = _parse_dt(issue["closedAt"]) if issue.get("closedAt") else None

    ext = sorted(
        (c for c in issue["comments"]
         if c["author"]["login"] != author and not _is_bot(c["author"]["login"])),
        key=lambda c: c["createdAt"],
    )

    first_comment_hours: Optional[float] = None
    if ext:
        first_comment_hours = _hours(_parse_dt(ext[0]["createdAt"]) - created)

    # time between consecutive external comments
    inter_comment_hours: list[float] = []
    for i in range(1, len(ext)):
        gap = _hours(_parse_dt(ext[i]["createdAt"]) - _parse_dt(ext[i-1]["createdAt"]))
        inter_comment_hours.append(gap)

    closer_type = issue.get("closer_type")  # 'PullRequest', 'Commit', or None
    state_reason = issue.get("stateReason")  # 'COMPLETED', 'NOT_PLANNED', or None

    # Classify how the issue was resolved (only meaningful for closed issues)
    if issue["state"] == "CLOSED":
        if closer_type in ("PullRequest", "Commit"):
            close_reason = "pr_or_commit"   # closed by a PR / merge commit — normal
        elif state_reason == "NOT_PLANNED":
            close_reason = "not_planned"    # explicitly marked won't-fix/duplicate
        else:
            close_reason = "manual"         # closed via UI with no PR or commit
    else:
        close_reason = None

    return {
        "number": issue["number"],
        "title": issue["title"],
        "author": author,
        "state": issue["state"],
        "state_reason": state_reason,
        "close_reason": close_reason,
        "created_at": issue["createdAt"],
        "closed_at": issue.get("closedAt"),
        "first_comment_hours": first_comment_hours,
        "inter_comment_hours": inter_comment_hours,
        "time_to_close_hours": _hours(closed_at - created) if closed_at else None,
        "external_comment_count": len(ext),
        "total_comment_count": len(issue["comments"]),
    }


# ── per-CI-run computation ────────────────────────────────────────────────────

def compute_ci_stats(runs: list[dict]) -> dict:
    """
    Analyse a list of ci runs and return per-branch + aggregate stats.

    Flakiness definition: a PR branch whose runs (on the same code, i.e. same
    branch name) include at least one failure AND at least one success.
    Because GitHub doesn't expose the triggering commit SHA in `gh run list`
    output, we use branch name as the grouping key — a conservative proxy.
    """
    pr_runs   = [r for r in runs if r["event"] == "pull_request"]
    main_runs = [r for r in runs if r["event"] == "push" and r["headBranch"] == "main"]

    by_branch: dict[str, list] = defaultdict(list)
    for r in pr_runs:
        by_branch[r["headBranch"]].append(r)

    multi_run   = {b: rr for b, rr in by_branch.items() if len(rr) > 1}
    flaky       = {b: rr for b, rr in multi_run.items()
                   if any(r["conclusion"] == "failure" for r in rr)
                   and any(r["conclusion"] == "success" for r in rr)}
    all_fail    = {b: rr for b, rr in multi_run.items()
                   if all(r["conclusion"] == "failure" for r in rr)}

    main_fail   = [r for r in main_runs if r["conclusion"] == "failure"]

    flaky_branches = [
        {
            "branch": b,
            "run_count": len(rr),
            "failures": sum(1 for r in rr if r["conclusion"] == "failure"),
            "successes": sum(1 for r in rr if r["conclusion"] == "success"),
            "pattern": [r["conclusion"] for r in sorted(rr, key=lambda r: r["createdAt"])],
        }
        for b, rr in sorted(flaky.items(), key=lambda x: -len(x[1]))
    ]

    return {
        "total_pr_runs": len(pr_runs),
        "total_main_runs": len(main_runs),
        "unique_pr_branches": len(by_branch),
        "multi_run_branches": len(multi_run),
        "flaky_branches": len(flaky),
        "all_fail_branches": len(all_fail),
        "pr_flakiness_rate_pct": round(len(flaky) / len(multi_run) * 100, 1) if multi_run else 0.0,
        "main_failure_count": len(main_fail),
        "main_failure_rate_pct": round(len(main_fail) / len(main_runs) * 100, 1) if main_runs else 0.0,
        "flaky_branch_detail": flaky_branches,
    }


# ── aggregation ───────────────────────────────────────────────────────────────

def _distribution(values: list[float]) -> dict:
    if not values:
        return {"count": 0}
    n = len(values)
    result: dict = {
        "count": n,
        "mean_h": round(mean(values), 2),
        "median_h": round(median(values), 2),
        "min_h": round(min(values), 2),
        "max_h": round(max(values), 2),
    }
    if n >= 4:
        result["p75_h"] = round(quantiles(values, n=4)[2], 2)
    if n >= 10:
        result["p90_h"] = round(quantiles(values, n=10)[8], 2)
    return result


def _comment_count_dist(issue_stats: list[dict]) -> dict:
    """Distribution of external comment counts across issues that have >=1 comment."""
    counts = [s["external_comment_count"] for s in issue_stats if s["external_comment_count"] > 0]
    if not counts:
        return {"count": 0}
    buckets = {
        "exactly_1": sum(1 for c in counts if c == 1),
        "2_to_3":    sum(1 for c in counts if 2 <= c <= 3),
        "4_to_9":    sum(1 for c in counts if 4 <= c <= 9),
        "10_plus":   sum(1 for c in counts if c >= 10),
    }
    return {
        "count": len(counts),
        "median": round(median(counts), 1),
        "mean": round(mean(counts), 1),
        "max": max(counts),
        "buckets": buckets,
    }


def aggregate_pr_stats(pr_stats: list[dict]) -> dict:
    first_vals = [s["first_review_hours"] for s in pr_stats if s["first_review_hours"] is not None]
    re_vals    = [h for s in pr_stats for h in s["re_review_hours"]]
    return {
        "total_prs": len(pr_stats),
        "prs_with_reviews": sum(1 for s in pr_stats if s["first_review_hours"] is not None),
        "prs_without_reviews": sum(1 for s in pr_stats if s["first_review_hours"] is None),
        "first_review": _distribution(first_vals),
        "re_review": _distribution(re_vals),
    }


def aggregate_issue_stats(issue_stats: list[dict]) -> dict:
    first_comment_vals = [s["first_comment_hours"] for s in issue_stats if s["first_comment_hours"] is not None]
    inter_comment_vals = [h for s in issue_stats for h in s["inter_comment_hours"]]
    close_vals         = [s["time_to_close_hours"] for s in issue_stats if s["time_to_close_hours"] is not None]
    closed             = [s for s in issue_stats if s["state"] == "CLOSED"]

    # Break down closed-no-comment issues by how they were closed
    closed_no_comment = [s for s in closed if s["first_comment_hours"] is None]
    return {
        "total_issues": len(issue_stats),
        "open_issues": sum(1 for s in issue_stats if s["state"] == "OPEN"),
        "closed_issues": len(closed),
        "issues_with_comments": sum(1 for s in issue_stats if s["first_comment_hours"] is not None),
        "issues_without_comments": sum(1 for s in issue_stats if s["first_comment_hours"] is None),
        "closed_no_comment_total": len(closed_no_comment),
        "closed_no_comment_by_pr_or_commit": sum(1 for s in closed_no_comment if s.get("close_reason") == "pr_or_commit"),
        "closed_no_comment_not_planned":     sum(1 for s in closed_no_comment if s.get("close_reason") == "not_planned"),
        "closed_no_comment_manual":          sum(1 for s in closed_no_comment if s.get("close_reason") == "manual"),
        "first_comment": _distribution(first_comment_vals),
        "inter_comment": _distribution(inter_comment_vals),
        "comment_count_dist": _comment_count_dist(issue_stats),
        "time_to_close": _distribution(close_vals),
    }


# ── output: PR ────────────────────────────────────────────────────────────────

def print_pr_summary(agg: dict, label: str = "") -> None:
    title = f"PR review latency — {label}" if label else "PR review latency"
    _hdr(title)
    print(f"  PRs in window:      {agg['total_prs']}")
    print(f"  PRs with reviews:   {agg['prs_with_reviews']}")
    print(f"  PRs never reviewed: {agg['prs_without_reviews']}")
    print()
    _print_dist(agg["first_review"], "Time to first review  (PR open → first external review)")
    _print_dist(agg["re_review"],    "Re-review turnaround  (CHANGES_REQUESTED → next review)")


def print_pr_comparison(before: dict, after: dict, bl: str, al: str) -> None:
    rows = [
        ("PRs analyzed",          before["total_prs"],                    after["total_prs"],                    False),
        ("PRs with reviews",      before["prs_with_reviews"],              after["prs_with_reviews"],              False),
        ("First review — median", before["first_review"].get("median_h"), after["first_review"].get("median_h"), True),
        ("First review — p75",    before["first_review"].get("p75_h"),    after["first_review"].get("p75_h"),    True),
        ("First review — p90",    before["first_review"].get("p90_h"),    after["first_review"].get("p90_h"),    True),
        ("First review — mean",   before["first_review"].get("mean_h"),   after["first_review"].get("mean_h"),   True),
        ("Re-review — median",    before["re_review"].get("median_h"),    after["re_review"].get("median_h"),    True),
        ("Re-review — p75",       before["re_review"].get("p75_h"),       after["re_review"].get("p75_h"),       True),
        ("Re-review — mean",      before["re_review"].get("mean_h"),      after["re_review"].get("mean_h"),      True),
    ]
    _print_cmp_table(bl, al, rows)


# ── output: issue ─────────────────────────────────────────────────────────────

def print_issue_summary(agg: dict, label: str = "") -> None:
    title = f"Issue traction — {label}" if label else "Issue traction"
    _hdr(title)
    cnc   = agg['closed_no_comment_total']
    by_pr = agg['closed_no_comment_by_pr_or_commit']
    notpl = agg['closed_no_comment_not_planned']
    manl  = agg['closed_no_comment_manual']

    print(f"  Issues in window:          {agg['total_issues']}")
    print(f"  Open:                      {agg['open_issues']}")
    print(f"  Closed:                    {agg['closed_issues']}")
    print(f"  With external comments:    {agg['issues_with_comments']}")
    print(f"  No external comments:      {agg['issues_without_comments']}")
    print(f"    ↳ closed by PR/commit:   {by_pr}  (visible cross-link — normal)")
    print(f"    ↳ closed as not-planned: {notpl}  (won't-fix/duplicate — expected)")
    print(f"    ↳ closed manually, no PR:{manl}  ← real concern")
    print()
    _print_dist(agg["first_comment"], "Time to first comment  (issue open → first external comment)")
    _print_dist(agg["inter_comment"], "Subsequent comment gap  (time between consecutive external comments)")

    ccd = agg.get("comment_count_dist", {})
    if ccd.get("count", 0) > 0:
        b = ccd["buckets"]
        n = ccd["count"]
        print(f"  Comment count distribution  (issues with >=1 comment, n={n})")
        print(f"    median {ccd['median']}   mean {ccd['mean']}   max {ccd['max']}")
        print(f"    exactly 1  : {b['exactly_1']:3d} ({b['exactly_1']/n*100:.0f}%)")
        print(f"    2–3        : {b['2_to_3']:3d} ({b['2_to_3']/n*100:.0f}%)")
        print(f"    4–9        : {b['4_to_9']:3d} ({b['4_to_9']/n*100:.0f}%)")
        print(f"    10+        : {b['10_plus']:3d} ({b['10_plus']/n*100:.0f}%)")
        print()

    _print_dist(agg["time_to_close"], "Time to close          (issue open → closed)")


def print_issue_comparison(before: dict, after: dict, bl: str, al: str) -> None:
    rows = [
        ("Issues total",           before["total_issues"],                      after["total_issues"],                      False),
        ("Issues w/ comments",     before["issues_with_comments"],               after["issues_with_comments"],               False),
        ("No-comment issues",      before["issues_without_comments"],            after["issues_without_comments"],            False),
        ("1st comment — median",   before["first_comment"].get("median_h"),     after["first_comment"].get("median_h"),     True),
        ("1st comment — p75",      before["first_comment"].get("p75_h"),        after["first_comment"].get("p75_h"),        True),
        ("1st comment — p90",      before["first_comment"].get("p90_h"),        after["first_comment"].get("p90_h"),        True),
        ("1st comment — mean",     before["first_comment"].get("mean_h"),       after["first_comment"].get("mean_h"),       True),
        ("Inter-comment — median", before["inter_comment"].get("median_h"),     after["inter_comment"].get("median_h"),     True),
        ("Inter-comment — mean",   before["inter_comment"].get("mean_h"),       after["inter_comment"].get("mean_h"),       True),
        ("Time to close — median", before["time_to_close"].get("median_h"),     after["time_to_close"].get("median_h"),     True),
        ("Time to close — p75",    before["time_to_close"].get("p75_h"),        after["time_to_close"].get("p75_h"),        True),
        ("Time to close — mean",   before["time_to_close"].get("mean_h"),       after["time_to_close"].get("mean_h"),       True),
    ]
    _print_cmp_table(bl, al, rows)


# ── output: CI ────────────────────────────────────────────────────────────────

def print_ci_summary(agg: dict, label: str = "") -> None:
    title = f"CI flakiness — {label}" if label else "CI flakiness"
    _hdr(title)
    print(f"  PR runs in window:            {agg['total_pr_runs']}")
    print(f"  Unique PR branches:           {agg['unique_pr_branches']}")
    print(f"  Multi-run PR branches:        {agg['multi_run_branches']}")
    print()
    print(f"  Flaky branches (fail+pass):   {agg['flaky_branches']} / {agg['multi_run_branches']}"
          f"  →  {agg['pr_flakiness_rate_pct']}%")
    print(f"  All-fail branches:            {agg['all_fail_branches']}")
    print()
    print(f"  Main push runs:               {agg['total_main_runs']}")
    print(f"  Main failures:                {agg['main_failure_count']} / {agg['total_main_runs']}"
          f"  →  {agg['main_failure_rate_pct']}%")
    print()
    top = agg.get("flaky_branch_detail", [])[:10]
    if top:
        print(f"  Top flaky branches (by run count):")
        for b in top:
            print(f"    [{b['run_count']:2d} runs | {b['failures']}✗ {b['successes']}✓] {b['branch']}")
    print()


def print_ci_comparison(before: dict, after: dict, bl: str, al: str) -> None:
    def _pct_delta(b, a):
        if b is None or a is None:
            return "n/a"
        diff = a - b
        sign = "+" if diff >= 0 else ""
        return f"{sign}{diff:.1f}pp"

    rows = [
        ("PR multi-run branches",   before["multi_run_branches"],       after["multi_run_branches"],       False),
        ("Flaky branches",          before["flaky_branches"],           after["flaky_branches"],           False),
        ("PR flakiness rate",       f"{before['pr_flakiness_rate_pct']}%", f"{after['pr_flakiness_rate_pct']}%",
         _pct_delta(before["pr_flakiness_rate_pct"], after["pr_flakiness_rate_pct"])),
        ("Main runs",               before["total_main_runs"],          after["total_main_runs"],          False),
        ("Main failure count",      before["main_failure_count"],       after["main_failure_count"],       False),
        ("Main failure rate",       f"{before['main_failure_rate_pct']}%", f"{after['main_failure_rate_pct']}%",
         _pct_delta(before["main_failure_rate_pct"], after["main_failure_rate_pct"])),
    ]
    col_w = 30
    print(f"\n{'═' * 78}")
    print(f"  Comparison: {bl}  →  {al}")
    print(f"{'═' * 78}")
    print(f"  {'Metric':<{col_w}} {'Before':>14} {'After':>14} {'Delta':>12}")
    print(f"  {'─'*col_w} {'─'*14} {'─'*14} {'─'*12}")
    for row in rows:
        label, b, a, d = row
        if isinstance(d, bool):  # is_time flag from other tables — not used here
            d = ""
        print(f"  {label:<{col_w}} {str(b):>14} {str(a):>14} {str(d):>12}")
    print()


# ── shared output helpers ─────────────────────────────────────────────────────

def _hdr(title: str) -> None:
    w = 64
    print(f"\n{'─' * w}")
    print(f"  {title}")
    print(f"{'─' * w}")


def _print_dist(d: dict, heading: str) -> None:
    n = d["count"]
    print(f"  {heading}  n={n}")
    if n == 0:
        print("    (no data)")
    else:
        print(f"    mean   {_fmt_hours(d['mean_h'])}")
        print(f"    median {_fmt_hours(d['median_h'])}")
        if "p75_h" in d:
            print(f"    p75    {_fmt_hours(d['p75_h'])}")
        if "p90_h" in d:
            print(f"    p90    {_fmt_hours(d['p90_h'])}")
        print(f"    min    {_fmt_hours(d['min_h'])}   max {_fmt_hours(d['max_h'])}")
    print()


def _print_cmp_table(bl: str, al: str, rows: list) -> None:
    def _delta(b, a) -> str:
        if b is None or a is None or b == 0:
            return "n/a"
        diff = a - b
        sign = "+" if diff >= 0 else ""
        pct = (diff / b) * 100
        return f"{sign}{_fmt_hours(diff)} ({sign}{pct:.0f}%)"

    col_w = 30
    print(f"\n{'═' * 78}")
    print(f"  Comparison: {bl}  →  {al}")
    print(f"{'═' * 78}")
    print(f"  {'Metric':<{col_w}} {'Before':>12} {'After':>12} {'Delta':>18}")
    print(f"  {'─'*col_w} {'─'*12} {'─'*12} {'─'*18}")
    for label, b, a, is_time in rows:
        b_str = _fmt_hours(b) if is_time else str(b)
        a_str = _fmt_hours(a) if is_time else str(a)
        d_str = _delta(b, a) if is_time else ""
        print(f"  {label:<{col_w}} {b_str:>12} {a_str:>12} {d_str:>18}")
    print()


# ── CSV helpers ───────────────────────────────────────────────────────────────

def pr_to_csv(pr_stats: list[dict]) -> str:
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(["number", "author", "created_at", "merged_at",
                "first_review_hours", "re_review_count", "re_review_hours",
                "review_count", "total_hours_open"])
    for s in pr_stats:
        w.writerow([s["number"], s["author"], s["created_at"], s.get("merged_at") or "",
                    "" if s["first_review_hours"] is None else round(s["first_review_hours"], 2),
                    len(s["re_review_hours"]),
                    ";".join(f"{h:.2f}" for h in s["re_review_hours"]),
                    s["review_count"],
                    "" if s["total_hours_open"] is None else round(s["total_hours_open"], 2)])
    return buf.getvalue()


def issue_to_csv(issue_stats: list[dict]) -> str:
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(["number", "author", "state", "created_at", "closed_at",
                "first_comment_hours", "inter_comment_hours",
                "time_to_close_hours", "external_comment_count", "total_comment_count"])
    for s in issue_stats:
        w.writerow([s["number"], s["author"], s["state"], s["created_at"], s.get("closed_at") or "",
                    "" if s["first_comment_hours"] is None else round(s["first_comment_hours"], 2),
                    ";".join(f"{h:.2f}" for h in s["inter_comment_hours"]),
                    "" if s["time_to_close_hours"] is None else round(s["time_to_close_hours"], 2),
                    s["external_comment_count"], s["total_comment_count"]])
    return buf.getvalue()


def ci_to_csv(agg: dict) -> str:
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(["branch", "run_count", "failures", "successes", "pattern"])
    for b in agg.get("flaky_branch_detail", []):
        w.writerow([b["branch"], b["run_count"], b["failures"], b["successes"],
                    "->".join(b["pattern"])])
    return buf.getvalue()


# ── PR subcommand handlers ────────────────────────────────────────────────────

def cmd_snapshot(args: argparse.Namespace) -> None:
    print(f"Fetching PRs from {args.repo} [{args.since} → {args.until}] ...", file=sys.stderr)
    prs = fetch_prs(args.repo, args.since, args.until, limit=args.limit)
    print(f"Found {len(prs)} closed PRs in window.", file=sys.stderr)
    stats = [compute_pr_stats(pr) for pr in prs]
    agg = aggregate_pr_stats(stats)
    if args.format == "json":
        print(json.dumps({"summary": agg, "per_pr": stats}, indent=2))
    elif args.format == "csv":
        print(pr_to_csv(stats))
    else:
        print_pr_summary(agg, label=f"{args.since} to {args.until}")
    if args.save:
        with open(args.save, "w") as f:
            json.dump({"window": {"since": args.since, "until": args.until},
                       "repo": args.repo, "kind": "pr", "summary": agg, "per_pr": stats}, f, indent=2)
        print(f"Snapshot saved → {args.save}", file=sys.stderr)


def _load_or_fetch_prs(repo, window, file_path, limit) -> tuple[dict, str]:
    if file_path:
        with open(file_path) as f:
            payload = json.load(f)
        agg = aggregate_pr_stats(payload["per_pr"])
        w = payload.get("window", {})
        label = f"{w.get('since','?')}:{w.get('until','?')} (from {file_path})"
        print_pr_summary(agg, label=label)
        return agg, label
    since, until = window.split(":")
    print(f"Fetching PRs from {repo} [{since} → {until}] ...", file=sys.stderr)
    prs = fetch_prs(repo, since, until, limit=limit)
    print(f"Found {len(prs)} closed PRs in window.", file=sys.stderr)
    stats = [compute_pr_stats(pr) for pr in prs]
    agg = aggregate_pr_stats(stats)
    print_pr_summary(agg, label=f"{since} to {until}")
    return agg, f"{since}:{until}"


def cmd_compare(args: argparse.Namespace) -> None:
    before_agg, bl = _load_or_fetch_prs(args.repo, args.before, args.before_file, args.limit)
    after_agg,  al = _load_or_fetch_prs(args.repo, args.after,  args.after_file,  args.limit)
    print_pr_comparison(before_agg, after_agg, bl, al)


# ── issue subcommand handlers ─────────────────────────────────────────────────

def cmd_issue_snapshot(args: argparse.Namespace) -> None:
    print(f"Fetching issues from {args.repo} [{args.since} → {args.until}] ...", file=sys.stderr)
    issues = fetch_issues(args.repo, args.since, args.until, limit=args.limit)
    print(f"Found {len(issues)} issues in window.", file=sys.stderr)
    stats = [compute_issue_stats(iss) for iss in issues]
    agg = aggregate_issue_stats(stats)
    if args.format == "json":
        print(json.dumps({"summary": agg, "per_issue": stats}, indent=2))
    elif args.format == "csv":
        print(issue_to_csv(stats))
    else:
        print_issue_summary(agg, label=f"{args.since} to {args.until}")
    if args.save:
        with open(args.save, "w") as f:
            json.dump({"window": {"since": args.since, "until": args.until},
                       "repo": args.repo, "kind": "issue", "summary": agg, "per_issue": stats}, f, indent=2)
        print(f"Snapshot saved → {args.save}", file=sys.stderr)


def _load_or_fetch_issues(repo, window, file_path, limit) -> tuple[dict, str]:
    if file_path:
        with open(file_path) as f:
            payload = json.load(f)
        agg = aggregate_issue_stats(payload["per_issue"])
        w = payload.get("window", {})
        label = f"{w.get('since','?')}:{w.get('until','?')} (from {file_path})"
        print_issue_summary(agg, label=label)
        return agg, label
    since, until = window.split(":")
    print(f"Fetching issues from {repo} [{since} → {until}] ...", file=sys.stderr)
    issues = fetch_issues(repo, since, until, limit=limit)
    print(f"Found {len(issues)} issues in window.", file=sys.stderr)
    stats = [compute_issue_stats(iss) for iss in issues]
    agg = aggregate_issue_stats(stats)
    print_issue_summary(agg, label=f"{since} to {until}")
    return agg, f"{since}:{until}"


def cmd_issue_compare(args: argparse.Namespace) -> None:
    before_agg, bl = _load_or_fetch_issues(args.repo, args.before, args.before_file, args.limit)
    after_agg,  al = _load_or_fetch_issues(args.repo, args.after,  args.after_file,  args.limit)
    print_issue_comparison(before_agg, after_agg, bl, al)


# ── CI subcommand handlers ────────────────────────────────────────────────────

def cmd_ci_snapshot(args: argparse.Namespace) -> None:
    print(f"Fetching CI runs from {args.repo} [{args.since} → {args.until}] "
          f"(fetching up to {args.limit} most-recent runs) ...", file=sys.stderr)
    runs = fetch_ci_runs(args.repo, args.since, args.until,
                         limit=args.limit, workflow=args.workflow)
    print(f"Found {len(runs)} completed pr-workflow runs in window.", file=sys.stderr)
    agg = compute_ci_stats(runs)
    if args.format == "json":
        print(json.dumps({"summary": agg, "runs": runs}, indent=2))
    elif args.format == "csv":
        print(ci_to_csv(agg))
    else:
        print_ci_summary(agg, label=f"{args.since} to {args.until}")
    if args.save:
        with open(args.save, "w") as f:
            json.dump({"window": {"since": args.since, "until": args.until},
                       "repo": args.repo, "workflow": args.workflow,
                       "kind": "ci", "summary": agg, "runs": runs}, f, indent=2)
        print(f"Snapshot saved → {args.save}", file=sys.stderr)


def _load_or_fetch_ci(repo, window, file_path, limit, workflow) -> tuple[dict, str]:
    if file_path:
        with open(file_path) as f:
            payload = json.load(f)
        agg = compute_ci_stats(payload["runs"])
        w = payload.get("window", {})
        label = f"{w.get('since','?')}:{w.get('until','?')} (from {file_path})"
        print_ci_summary(agg, label=label)
        return agg, label
    since, until = window.split(":")
    print(f"Fetching CI runs from {repo} [{since} → {until}] ...", file=sys.stderr)
    runs = fetch_ci_runs(repo, since, until, limit=limit, workflow=workflow)
    print(f"Found {len(runs)} completed runs in window.", file=sys.stderr)
    agg = compute_ci_stats(runs)
    print_ci_summary(agg, label=f"{since} to {until}")
    return agg, f"{since}:{until}"


def cmd_ci_compare(args: argparse.Namespace) -> None:
    before_agg, bl = _load_or_fetch_ci(args.repo, args.before, args.before_file, args.limit, args.workflow)
    after_agg,  al = _load_or_fetch_ci(args.repo, args.after,  args.after_file,  args.limit, args.workflow)
    print_ci_comparison(before_agg, after_agg, bl, al)


# ── CLI wiring ────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    sub = parser.add_subparsers(dest="cmd")

    def _add_common(p):
        p.add_argument("--repo", default="agent-substrate/substrate",
                       help="GitHub repo owner/name (default: agent-substrate/substrate)")
        p.add_argument("--limit", type=int, default=500,
                       help="Max items to fetch per window (default: 500; use 1000+ for ci-snapshot)")
        p.add_argument("--format", choices=["table", "json", "csv"], default="table")

    def _add_window(p):
        p.add_argument("--since", required=True, metavar="YYYY-MM-DD")
        p.add_argument("--until", required=True, metavar="YYYY-MM-DD")
        p.add_argument("--save", metavar="FILE",
                       help="Save snapshot JSON for later --before-file/--after-file use")

    def _add_cmp(p):
        p.add_argument("--before", metavar="SINCE:UNTIL")
        p.add_argument("--before-file", metavar="FILE")
        p.add_argument("--after",  metavar="SINCE:UNTIL")
        p.add_argument("--after-file",  metavar="FILE")

    # PR
    p = sub.add_parser("snapshot",      help="PR review latency for a date window")
    _add_common(p); _add_window(p); p.set_defaults(func=cmd_snapshot)

    p = sub.add_parser("compare",       help="Compare PR review latency across two windows")
    _add_common(p); _add_cmp(p); p.set_defaults(func=cmd_compare, _p=p)

    # Issue
    p = sub.add_parser("issue-snapshot", help="Issue traction stats for a date window")
    _add_common(p); _add_window(p); p.set_defaults(func=cmd_issue_snapshot)

    p = sub.add_parser("issue-compare",  help="Compare issue traction across two windows")
    _add_common(p); _add_cmp(p); p.set_defaults(func=cmd_issue_compare, _p=p)

    # CI
    p = sub.add_parser("ci-snapshot",   help="CI flakiness stats for a date window")
    _add_common(p); _add_window(p)
    p.add_argument("--workflow", default="pr-workflow",
                   help="Workflow filename/name to analyse (default: pr-workflow)")
    p.set_defaults(func=cmd_ci_snapshot)

    p = sub.add_parser("ci-compare",    help="Compare CI flakiness across two windows")
    _add_common(p); _add_cmp(p)
    p.add_argument("--workflow", default="pr-workflow")
    p.set_defaults(func=cmd_ci_compare, _p=p)

    args = parser.parse_args()

    if args.cmd is None:
        parser.print_help()
        sys.exit(0)

    if args.cmd in ("compare", "issue-compare", "ci-compare"):
        p = args._p
        if not (args.before or args.before_file):
            p.error("provide --before SINCE:UNTIL or --before-file FILE")
        if not (args.after or args.after_file):
            p.error("provide --after SINCE:UNTIL or --after-file FILE")

    args.func(args)


if __name__ == "__main__":
    main()
