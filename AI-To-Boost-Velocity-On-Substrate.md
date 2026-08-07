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

### 1. CI / Test Flakiness — Short Term

Wire `hack/metrics/ci-failure-analysis.py` into a GitHub Actions scheduled workflow that runs every N hours and acts on its own findings. The agent operates exactly like a human contributor — it can only open issues and send PRs. It has no direct write access to the repo.

**Per run:**
1. Run `ci-failure-analysis.py` to classify the latest failed runs
2. For each newly seen named test failure not already in `hack/metrics/flaky-registry.json`:
   - Open a GitHub issue with the test name, failure log excerpt, and fail count
   - Record the issue number in `flaky-registry.json`
3. For the top-3 unresolved unit test flakes (by frequency):
   - Agent reads the test source and failure log
   - Proposes a fix (race fix, deterministic setup, retry guard, etc.)
   - Opens a PR linked to the issue
4. For `no free workers` failures: open a single tracking issue (not per-test) flagging the envtest setup as the root cause — needs human investigation, not a code fix
5. Close registry entries for tests that have been clean for 10+ consecutive runs

**Current known flakes to seed the registry (Aug 2026):**

| Test | Failures | Category |
|---|---|---|
| `TestDurableDirLifecycle` | 6 | unit |
| `TestActorLifecycle` | 4 | unit |
| `TestMultipleDurableDirLifecycle` | 2 | unit |
| `TestSyncer_UpdateWorker_RetryOnVersionConflict` | 2 | unit |
| `TestLoaderConcurrentHandshakes` | 2 | unit |

**Success metric:** main CI failure rate drops from 23.6% toward 0. Measure with `pr-review-metrics.py ci-compare` after 30 days.

---

### 2. PR Review — Short Term

Work with TLs to define and build a **PR review skill** — a Claude Code slash command that, given a PR, produces a structured review: summary of changes, correctness concerns, test coverage gaps, and a recommendation (approve / request changes / needs discussion).

> **Note:** A `/review` skill already exists in Claude Code for GitHub PRs. Evaluate whether it fits the substrate codebase's conventions (Go, Kubernetes, actor model patterns) or needs a project-specific variant with substrate-specific guidance baked in.

Once the skill is validated by TLs, wire it into a GitHub Actions workflow triggered on PR open and push:

```yaml
on:
  pull_request:
    types: [opened, synchronize]
```

**Per trigger:**
1. Agent runs the PR review skill against the diff
2. Posts the review as a PR comment (not a blocking review — advisory only until quality is validated)
3. If the PR has no linked issue, flags it per the PR etiquette guidelines (PR #722)

**Success metric:** % of PRs with at least one review increases from 77% → 90%+. The agent covers the long tail of PRs that currently get no human attention.

---

### 3. Issue Triage — Short Term

Work with TLs to define and build an **issue triage skill** — there is no existing one. The skill, given an issue, should: identify if it's a duplicate, suggest relevant labels, ask for missing repro information, and post an initial acknowledgement so the reporter knows it was seen.

> **Note:** No issue triage skill currently exists in Claude Code. This needs to be created. The skill should be substrate-aware: know the component areas (atelet, atenet, ateapi, ateomnet, etc.) so it can label and route correctly.

Once the skill is ready, wire it into a GitHub Actions workflow triggered on issue open and edit:

```yaml
on:
  issues:
    types: [opened, edited]
```

**Per trigger:**
1. Agent runs the issue triage skill
2. Posts a triage comment: acknowledgement, labels applied, follow-up questions if repro is missing, duplicate link if found
3. Assigns to the relevant component owner if the area is identifiable

**Success metric:** issue engagement rate increases from 48% → 80%+. The agent ensures every issue gets at least one substantive response within minutes of filing.

---

## Long Term — Metric-Driven Autonomous Contributors

This applies across all three areas. Rather than scripting specific actions, give each agent a high-level metric and let it figure out how to move it. The agent behaves like any other contributor: it reads the codebase, writes scripts, files issues, and opens PRs. It cannot merge its own PRs, cannot push to protected branches, and cannot take any action a regular external contributor couldn't take. The existing review process is the safety layer.

| Agent | Metric | Allowed actions |
|---|---|---|
| Flakiness agent | Main CI failure rate < 2% | File issues, open PRs fixing failing tests |
| Coverage agent | No net regression in test coverage | Open PRs adding missing test cases |
| Review agent | p90 first-review < 24h | Review PRs, ping stale ones, suggest reviewers |
| Triage agent | < 20% of issues uncommented after 48h | Triage, label, comment, surface duplicates |

Each agent runs on a schedule, measures its own metric, decides whether to act, and acts if the metric is off-target. Maintainers review and merge (or reject) the output like any other contribution.

**The metric is the contract, not the implementation.** Agents can create new scripts, refactor test utilities, restructure test setup — whatever moves the number. This avoids the brittleness of scripted automation and lets agents adapt as the codebase evolves.

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
