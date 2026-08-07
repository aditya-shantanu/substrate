# AI To Boost Velocity On Substrate

**Baseline captured:** 2026-08-07  
**Window:** Full repo history — 2026-05-13 to 2026-08-07  
**Fork:** [github.com/aditya-shantanu/substrate](https://github.com/aditya-shantanu/substrate) · branch: [`docs/velocity-metrics`](https://github.com/aditya-shantanu/substrate/tree/docs/velocity-metrics)  
**Scripts:** [`hack/metrics/pr-review-metrics.py`](https://github.com/aditya-shantanu/substrate/blob/docs/velocity-metrics/hack/metrics/pr-review-metrics.py) · [`hack/metrics/ci-failure-analysis.py`](https://github.com/aditya-shantanu/substrate/blob/docs/velocity-metrics/hack/metrics/ci-failure-analysis.py)

---

## Summary

### 1. CI / Test Flakiness ⚠️ Most Urgent

Flakiness is a direct velocity tax on every contributor and the most acute problem right now.

- **52.9% PR branch flakiness rate.** Over half of PR branches that trigger more than one CI run have a pattern of fail → pass on the same code. Contributors routinely re-run CI without changes just to get a green signal.
- **Main branch is broken 23.6% of the time.** Roughly 1 in 4 merges to main produces a failing CI run, forcing every subsequent PR author to triage "is this me or pre-existing?"
- **Root cause is almost certainly the e2e tests.** The workflow runs a full kind cluster with micro-VM assets, KVM access, and two Kubernetes network stacks on GitHub's free `ubuntu-latest` runners — highly sensitive to runner state, disk, and cache.

### 2. Issue Velocity

After correcting for workflow artifacts, issue health looks better than the raw numbers suggested — but one real gap remains.

- **38% of closed issues (37/98) are self-fixed**: the author filed the issue and their own PR closed it, a direct consequence of the PR template's issue-first requirement. These don't need triage. Stripping them out, the actual community issue queue is smaller and more engaged than the raw stats showed.
- **Only 6 issues were genuinely closed with no explanation** — no comment, no PR link, no reason. Not a systemic problem.
- **Issue engagement rate shows 48% of recent issues receive no external comment**, but this is inflated by self-fixed issues. The underlying concern is real: as issue volume grows, maintainer response bandwidth isn't keeping pace with the community queue.
- **43% of community issues that get any comment receive only 1.** This is less alarming than it sounds — many resolve quickly via a single "fixed in PR #X" response. The ones to watch are open issues with 1 comment and no follow-up.

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

**Success metric:** community issue engagement rate (excluding self-fixed issues) increases from ~63% → 85%+. The agent ensures every community issue gets at least one substantive response within minutes of filing.

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

#### Self-Fixed vs Community Issues

38% of closed issues (37/98) are **self-fixed**: the author filed the issue and their own PR closed it — a workflow artifact of the issue-first PR template. These require no triage. The remaining 61 closed issues are community issues where someone other than the author resolved it.

| Category | Count | Notes |
|---|---|---|
| Closed issues total | 98 | |
| Self-fixed (author filed + fixed) | 37 (38%) | PR template artifact |
| Community-resolved | 61 (62%) | Someone else fixed or reviewed |

#### First Comment

Time from issue opened to the first non-author, non-bot comment.

**Overall (282 issues, all time)**

| Metric | All issues | Community only (excl. self-fixed) |
|---|---|---|
| Issues with ≥1 external comment | 171 / 282 (61%) | 155 / 245 (63%) |
| Issues with no external comment | 111 / 282 (39%) | 90 / 245 (37%) |
| Median | 18.0h | 21.0h |
| p75 | 3.7d | 4.5d |
| p90 | 16.5d | 17.6d |
| Mean | 5.3d | 5.8d |

**Trend (all issues)**

| Metric | May–Jun (n=93) | Jul–Aug (n=77) | Change |
|---|---|---|---|
| Issues with comments | 93/119 (78%) | 77/161 (48%) | -30pp |
| Median | 23.8h | 17.0h | -28% |
| p75 | 9.6d | 2.4d | -75% |
| Mean | 8.0d | 2.2d | -73% |

Note: the apparent drop in engagement rate (78% → 48%) is partly driven by a growing share of self-fixed issues in the recent period, which never need an external comment.

#### Subsequent Comments

Comment count per issue (community issues with ≥1 external comment, n=155):

| Comments per issue | Count | Share |
|---|---|---|
| Exactly 1 | 67 | 43% |
| 2–3 | 47 | 30% |
| 4–9 | 34 | 22% |
| 10+ | 7 | 5% |
| Median / Mean / Max | 2.0 / 3.0 / 20 | — |

43% of community issues that receive any engagement get exactly 1 comment. This is not necessarily alarming — many are resolved with a single "fixed in PR #X" acknowledgement. The watch list is open issues with 1 stale comment.

**Gap between consecutive external comments** (n=356 pairs)

| Metric | Value |
|---|---|
| Median | 15.4h |
| p75 | 3.3d |
| p90 | 12.0d |
| Mean | 4.6d |
| Min / Max | 0m / 65.2d |

#### Time to Close

Time from issue opened to issue closed (closed issues only, n=98).

| Close reason | Count | Notes |
|---|---|---|
| Self-fixed (author's own PR) | 37 (38%) | PR template artifact — normal |
| Closed by someone else's PR/commit | 13 (13%) | Cross-link visible — normal |
| Closed as not-planned | 0 | — |
| Closed manually, no PR, no comment | **6 (6%)** | Real concern — no explanation |

| Metric | Value |
|---|---|
| Median | 3.7d |
| p75 | 11.0d |
| p90 | 41.0d |
| Mean | 10.0d |
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
