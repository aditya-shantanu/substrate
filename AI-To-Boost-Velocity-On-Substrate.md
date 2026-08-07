# AI To Boost Velocity On Substrate

**Baseline captured:** 2026-08-07  
**Window:** Full repo history — 2026-05-13 to 2026-08-07  
**Script:** `hack/metrics/pr-review-metrics.py`

---

## Summary

### 1. CI / Test Flakiness ⚠️ Most Urgent

Flakiness is a direct velocity tax on every contributor and the most acute problem right now.

- **52.9% PR branch flakiness rate.** Over half of PR branches that trigger more than one CI run have a pattern of fail → pass on the same code. Contributors routinely re-run CI without changes just to get a green signal.
- **Main branch is broken 23.6% of the time.** Roughly 1 in 4 merges to main produces a failing CI run, forcing every subsequent PR author to triage "is this me or pre-existing?"
- **Root cause is almost certainly the e2e tests.** The workflow runs a full kind cluster with micro-VM assets, KVM access, and two Kubernetes network stacks on GitHub's free `ubuntu-latest` runners — highly sensitive to runner state, disk, and cache.

### 2. Issue Velocity

The speed of engagement on issues that do get attention is improving. The structural problem is coverage — and it is getting worse as volume grows.

- **Issue engagement rate collapsed from 78% → 48%** as issue volume grew 35% from May–Jun to Jul–Aug. The absolute number of issues receiving any comment barely changed while the queue grew — maintainer bandwidth is not scaling.
- **40% of closed issues were closed without any comment.** Contributors get no explanation, which hurts trust and community growth.
- **45% of issues that do get a comment receive only 1.** Single-touch triage is the norm, not sustained discussion.

### 3. PR Velocity

The least critical area — direction is positive. First-review latency improved 41% (mean) and re-review turnaround improved 57% over the past two months. Two gaps remain:

- **23% of PRs receive no review at all.** Nearly 1 in 4 closed PRs was merged or closed without a single external review event. This is a governance gap, not a speed gap.
- **Long tail: p90 first-review is 3.3 days.** The median looks healthy at 6h, but 10% of PRs wait more than 3 days — a small number of PRs drag the mean (29h) well above the median.

---

## Plan

### CI / Test Flakiness

The failure analysis script (`hack/metrics/ci-failure-analysis.py`) classifies CI failures by root cause rather than just counting branch-level flakiness. Run it first to confirm the breakdown hasn't shifted before acting:

```bash
python3 hack/metrics/ci-failure-analysis.py \
  --repo agent-substrate/substrate \
  --fetch-limit 500 --sample 40 \
  --save /tmp/ci-failure-breakdown.json
```

**Current breakdown (40 sampled failed runs, Aug 2026):**

| Category | Count | Action |
|---|---|---|
| Named Go test failures | 20 / 40 (50%) | Fix the specific tests below |
| No free workers (envtest contention) | 12 / 40 (30%) | Fix test setup, not the tests |
| E2e timeout / 503 | 2 / 40 (5%) | Retry wrapper or runner improvement |
| Unclassified | 6 / 40 (15%) | Investigate manually |

**Specific tests failing (by frequency):**
- `TestDurableDirLifecycle` — 6×
- `TestActorLifecycle` — 4×
- `TestMultipleDurableDirLifecycle` — 2×
- `TestSyncer_UpdateWorker_RetryOnVersionConflict` — 2×
- `TestLoaderConcurrentHandshakes` — 2×

**Planned agent pipeline (per N hours):**

```
ci-failure-analysis.py
  → for each new named test failure not in flaky-registry.json:
      file GitHub issue (with failure log excerpt)
      add to registry (test → issue number)
  → for top-3 unit test flakes:
      agent reads test + failure log → opens PR with fix
  → for "no free workers" failures:
      fix is in test setup (not the test) → separate manual investigation
  → update registry (close issues for tests clean for 10+ runs)
```

<!-- Fill in remaining sections: Issue Velocity plan, PR Velocity plan -->

---

## Appendix

### A1. PR Velocity — Detail

#### First Review

Time from PR opened to the first non-author review event (any state: approved, changes requested, or comment).

**Overall (409 closed PRs, all time)**

| Metric | Value |
|---|---|
| PRs with at least one review | 316 / 409 (77%) |
| PRs never reviewed | 93 / 409 (23%) |
| Median | 6.0h |
| p75 | 25.8h |
| p90 | 3.3d |
| Mean | 29.1h |
| Min / Max | 1m / 26.5d |

**Trend**

| Metric | May–Jun (n=164) | Jul–Aug (n=152) | Change |
|---|---|---|---|
| PRs with reviews | 164/220 (75%) | 152/190 (80%) | +5pp |
| Median | 6.5h | 6.0h | -8% |
| p75 | 30.1h | 24.5h | -19% |
| p90 | 4.2d | 2.8d | -34% |
| Mean | 36.2h | 21.3h | -41% |

#### Subsequent Review (Re-review)

Time from a `CHANGES_REQUESTED` review to the next review — how long it takes a reviewer to return after the author addresses feedback.

**Overall (22 re-review cycles, all time)**

| Metric | Value |
|---|---|
| Median | 9.3h |
| p75 | 20.3h |
| p90 | 3.3d |
| Mean | 23.9h |
| Min / Max | 0m / 8.5d |

**Trend**

| Metric | May–Jun (n=6) | Jul–Aug (n=16) | Change |
|---|---|---|---|
| Median | 10.7h | 9.3h | -13% |
| p75 | 2.7d | 23.1h | -64% |
| Mean | 40.8h | 17.6h | -57% |

Sample is small (only PRs that received a `CHANGES_REQUESTED` review) — treat directionally.

---

### A2. Issue Velocity — Detail

#### First Comment

Time from issue opened to the first non-author, non-bot comment.

**Overall (280 issues, all time)**

| Metric | Value |
|---|---|
| Issues with at least one external comment | 170 / 280 (61%) |
| Issues with no external comment | 110 / 280 (39%) |
| Median | 18.5h |
| p75 | 3.7d |
| p90 | 16.7d |
| Mean | 5.4d |
| Min / Max | 1m / 63.7d |

**Trend**

| Metric | May–Jun (n=93) | Jul–Aug (n=77) | Change |
|---|---|---|---|
| Issues with comments | 93/119 (78%) | 77/161 (48%) | -30pp |
| Median | 23.8h | 17.0h | -28% |
| p75 | 9.6d | 2.4d | -75% |
| Mean | 8.0d | 2.2d | -73% |

#### Subsequent Comments

Comment count per issue (170 issues with ≥1 external comment):

| Comments per issue | Count | Share |
|---|---|---|
| Exactly 1 | 76 | 45% |
| 2–3 | 48 | 28% |
| 4–9 | 38 | 22% |
| 10+ | 8 | 5% |
| Median / Mean / Max | 2.0 / 3.1 / 28 | — |

**Gap between consecutive external comments** (n=353 pairs)

| Metric | Value |
|---|---|
| Median | 14.8h |
| p75 | 3.1d |
| p90 | 11.7d |
| Mean | 4.5d |
| Min / Max | 0m / 65.2d |

#### Time to Close

Time from issue opened to issue closed (closed issues only).

**Overall (97 closed issues, all time)**

| Metric | Value |
|---|---|
| Closed without any comment | 39 / 97 (40%) |
| Median | 3.6d |
| p75 | 11.2d |
| p90 | 41.0d |
| Mean | 10.1d |
| Min / Max | 0m / 69.3d |

**Trend**

| Metric | May–Jun (n=49) | Jul–Aug (n=48) | Change |
|---|---|---|---|
| Median | 6.9d | 2.0d | -71% |
| p75 | 24.0d | 6.2d | -74% |
| Mean | 16.4d | 3.6d | -78% |

---

### A3. CI / Test Flakiness — Detail

**Data window:** Jul 29 – Aug 7, 2026 (391 completed runs).  
**Workflow:** `pr-workflow` — runs `go test -race ./...`, root-gated tests, `hack/verify-all.sh`, and a full e2e matrix on a kind cluster with micro-VM + gVisor, two auth modes (cert/token).

#### PR Branch Flakiness

A branch is "flaky" if its `pr-workflow` had both a `failure` and a `success` run on the same code — i.e., it needed a re-run to go green.

| Metric | Value |
|---|---|
| PR branches with 2+ CI runs | 68 |
| Flaky branches (fail+success) | 36 / 68 (**52.9%**) |
| All-fail branches | 3 / 68 (4%) |

Top flaky branches (by run count):

| Branch | Runs | Failures | Successes |
|---|---|---|---|
| feature/actor-crashes-telemetry | 25 | 11 | 14 |
| feature/eligible-workers-telemetry | 14 | 6 | 8 |
| issue-706-atunnel-identity | 12 | 4 | 8 |
| feature/router-telemetry | 11 | 4 | 7 |
| external-volumes-for-agent-branch | 9 | 6 | 3 |
| endpointGRPCRes | 7 | 6 | 1 |

#### Main Branch Health

Every push to `main` triggers `pr-workflow`. A failing main run contaminates the CI baseline for all PRs that follow.

| Metric | Value |
|---|---|
| Main push runs | 72 |
| Failed | 17 / 72 (**23.6%**) |
| Passed | 55 / 72 (76.4%) |

#### Root Cause (Likely)

The e2e test matrix involves kind cluster creation, KVM access, micro-VM asset caching, cloud-hypervisor, and full Kubernetes networking — all on GitHub's free `ubuntu-latest` runners. These are highly sensitive to runner state, disk space, network latency, and cache hits. The unit tests (`go test -race ./...`) are unlikely culprits; infrastructure-dependent e2e tests are almost certainly the primary source.

#### Trend

The `gh run list` API returns the N most-recent runs, capping usable history at ~10 days with `--limit 1000`. A longer-running measurement will be needed to track improvement over time. Use `ci-snapshot` + `ci-compare` (see below) as the baseline accumulates.

---

### A4. How to Remeasure

Scripts live in `hack/metrics/`. All subcommands support `--save FILE` to snapshot data for later comparison with `--before-file` / `--after-file`.

```bash
# Adjust dates to your post-change measurement window
AFTER_SINCE=2026-09-01
AFTER_UNTIL=2026-09-30

# Capture after-snapshots
python3 hack/metrics/pr-review-metrics.py snapshot \
  --repo agent-substrate/substrate \
  --since $AFTER_SINCE --until $AFTER_UNTIL \
  --save pr-after.json

python3 hack/metrics/pr-review-metrics.py issue-snapshot \
  --repo agent-substrate/substrate \
  --since $AFTER_SINCE --until $AFTER_UNTIL \
  --save issue-after.json

# CI: increase --limit to cover longer windows
python3 hack/metrics/pr-review-metrics.py ci-snapshot \
  --repo agent-substrate/substrate \
  --since $AFTER_SINCE --until $AFTER_UNTIL \
  --limit 2000 \
  --save ci-after.json

# Compare against today's baselines
python3 hack/metrics/pr-review-metrics.py compare \
  --repo agent-substrate/substrate \
  --before-file /tmp/substrate-all-prs-baseline.json \
  --after-file pr-after.json

python3 hack/metrics/pr-review-metrics.py issue-compare \
  --repo agent-substrate/substrate \
  --before-file /tmp/substrate-all-issues-baseline.json \
  --after-file issue-after.json

python3 hack/metrics/pr-review-metrics.py ci-compare \
  --repo agent-substrate/substrate \
  --before-file /tmp/substrate-ci-baseline.json \
  --after-file ci-after.json
```

Baseline snapshots (local):
- PRs:    `/tmp/substrate-all-prs-baseline.json`
- Issues: `/tmp/substrate-all-issues-baseline.json`
- CI:     `/tmp/substrate-ci-baseline.json`
