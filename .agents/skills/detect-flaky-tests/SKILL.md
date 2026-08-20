---
name: detect-flaky-tests
description: >
  Detects flaky Go tests by analyzing GitHub Actions workflow runs across the last 7 days
  and all PRs. For each newly-detected flaky test, opens a GitHub issue with full evidence
  and a draft fix PR. Does not touch BigQuery, dashboards, or any external storage —
  those are cron-job concerns layered on top.
---

# Detect Flaky Tests

A test is **flaky** when it produces both PASS and FAIL outcomes across multiple independent
CI runs in the last 7 days, with no code change to that test's package explaining the
inconsistency. Cross-PR analysis provides the strongest signal: if the same test fails on
PR-A but passes on PR-B, that inconsistency is almost certainly non-determinism, not a
legitimate regression.

## Flakiness threshold (keeps false-positive rate low)

A test is flagged only when **all three** conditions hold in the 7-day window:

| Condition | Rationale |
|---|---|
| `fail_count >= 2` | One failure could be infra noise |
| `pass_count >= 2` | One pass could be a pre-fix lucky run |
| `0.05 < fail_rate < 0.95` | Outside this band it is either reliably broken or reliably passing |

## Step 1 — Collect workflow run IDs (last 7 days)

```bash
# List completed runs of pr-workflow for the last 7 days
SINCE=$(date -u -v-7d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ)
gh api "repos/agent-substrate/substrate/actions/workflows/pr-workflow.yaml/runs?status=completed&per_page=100&created=>=$SINCE" \
  --jq '.workflow_runs[] | {id: .id, conclusion: .conclusion, head_sha: .head_sha, created_at: .created_at}'
```

Collect the run IDs. You only need runs that completed (success or failure — both contain test output).

## Step 2 — Download and parse test logs per run

For each run ID, find the `run-tests` job and download its log:

```bash
# Get job ID for the run-tests job
gh api "repos/agent-substrate/substrate/actions/runs/<RUN_ID>/jobs" \
  --jq '.jobs[] | select(.name == "run-tests") | {id: .id, conclusion: .conclusion}'

# Download the log (returns a redirect to a zip)
gh api "repos/agent-substrate/substrate/actions/jobs/<JOB_ID>/logs" > /tmp/run_<RUN_ID>.log
```

Parse `go test -v` output. Each test result appears as one of:
```
--- PASS: TestFoo (0.12s)
--- FAIL: TestBar (1.23s)
--- SKIP: TestBaz (0.00s)
```

Extract `(PASS|FAIL)` and the test name. Ignore SKIP. The package is on the preceding
`=== RUN   TestFoo` line's context or from `ok  \tgithub.com/agent-substrate/substrate/...`
lines above.

Quick extraction pattern (adapt as needed):
```bash
grep -E '^--- (PASS|FAIL): ' /tmp/run_<RUN_ID>.log \
  | awk '{print $2, $3}' \
  | sed 's/://'
```

## Step 3 — Aggregate across all runs

Build a per-test-name table:

| test_name | package | fail_count | pass_count | total_runs |
|---|---|---|---|---|

Apply the threshold: `fail_count >= 2 AND pass_count >= 2 AND 0.05 < fail_rate < 0.95`.

## Step 4 — Deduplicate against open issues

Before creating a new issue, check whether one already exists:

```bash
gh issue list \
  --repo agent-substrate/substrate \
  --state open \
  --label "kind/bug,area/tests" \
  --search "flaky: <TEST_NAME>" \
  --json number,title
```

Only proceed to issue creation and fix PR if no open issue title matches
`flaky: <TEST_NAME>`.

## Step 5 — Create a GitHub issue for each new flaky test

```bash
gh issue create \
  --repo agent-substrate/substrate \
  --title "flaky: <TEST_NAME>" \
  --label "kind/bug,area/tests" \
  --body "$(cat <<'BODY'
## Flaky test detected

**Test:** `<TEST_NAME>`
**Package:** `<PACKAGE>`

### Evidence (last 7 days)

| Metric | Value |
|---|---|
| Runs analysed | <TOTAL_RUNS> |
| Failures | <FAIL_COUNT> |
| Passes | <PASS_COUNT> |
| Flake rate | <FLAKE_RATE>% |

### Failing run examples

<links to 2-3 failing runs>

### Passing run examples

<links to 1-2 passing runs>

### Next steps

A draft fix PR will be (or has been) opened by the detect-flaky-tests agent.
If the fix is incorrect or the flake is environment-specific, close this issue
and add a comment explaining why.
BODY
)"
```

Record the issue URL for the fix PR description.

## Step 6 — Open a draft fix PR for each new flaky test

Read the test source file. Diagnose the likely cause using the patterns below, then
apply the fix in a new branch and open a draft PR.

### Common Go flakiness patterns and fixes

| Pattern | Symptoms | Fix |
|---|---|---|
| **Timing / sleep** | Test sleeps for fixed duration then asserts state | Replace `time.Sleep` with `require.Eventually` or `testutil.WaitFor` |
| **Shared global state** | Test modifies a package-level var without restoring it | Move state into test-local var; use `t.Cleanup` to restore |
| **Port conflicts** | Test binds `:0` but then hardcodes the port in a second connection | Use the listener's actual address from `ln.Addr()` |
| **Goroutine leak** | Test spawns goroutines that outlive the test and race the next one | Add `t.Cleanup(cancel)` and wait for goroutines to exit |
| **File system races** | Parallel tests write to the same temp path | Use `t.TempDir()` (unique per test) instead of a shared path |
| **Context not cancelled** | Long-running operation not stopped; bleeds into the next test | Pass `t.Context()` (Go 1.21+) or create + cancel a context in `t.Cleanup` |
| **Order dependency** | Test assumes prior test ran and left data | Make each test self-contained; use `t.Cleanup` to reset state |

Steps for the fix PR:

1. Create a branch: `fix/flaky-<test-name-kebab>` from `main`
2. Edit the test file to apply the fix
3. Commit: `fix(tests): resolve flakiness in <TestName>\n\nFixes #<issue_number>`
4. Open a **draft** PR:

```bash
gh pr create \
  --repo agent-substrate/substrate \
  --title "fix(tests): resolve flakiness in <TestName>" \
  --draft \
  --body "$(cat <<'BODY'
## Summary

Fixes the flaky test `<TestName>` in `<package>`.

**Root cause:** <one sentence>
**Fix:** <one sentence>

Closes #<issue_number>

## Evidence

Flake rate over last 7 days: <FLAKE_RATE>%

Failing runs: <links>
Passing runs: <links>

## Test plan

- [ ] Run `go test -race -count=10 ./path/to/package/...` locally to verify the fix is
      stable under repeated execution
BODY
)"
```

## Step 7 — Report

Output a summary table of what was done:

```
| Test | Package | Flake rate | Issue | Fix PR | Action |
|---|---|---|---|---|---|
| TestFoo | pkg/foo | 40% | #NNN | #MMM | created |
| TestBar | pkg/bar | 25% | #OOO | — | issue only (existing PR) |
```

If no flaky tests are found, output: `No new flaky tests detected in the last 7 days.`

## Notes

- This skill does NOT write to BigQuery, update a dashboard, or perform any storage
  operations. Those are handled by the cron job that invokes this skill.
- Do not flag tests that only fail on `e2e-test` jobs (environment-specific failures
  are expected); focus on `run-tests` job output unless told otherwise.
- If log download fails for a run (e.g. logs expired), skip that run and note it in
  the report.
- The `--add-label` flag may fail if `area/tests` does not exist on the repo; fall
  back to omitting labels and add a comment instead.
