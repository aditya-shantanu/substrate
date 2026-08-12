---
name: triage-issues
description: Triages open GitHub issues by applying the correct labels. Use when asked to triage issues, label issues, or organize the issue backlog. Processes unlabeled issues first, then reviews labeled issues for correctness.
---

# Triage Issues

Triage means: read each issue, decide the right labels from the taxonomy below, and
apply them with `gh issue edit`. Do not remove labels a human has already added unless
they are clearly wrong. Do not add priority labels unless the issue body or discussion
makes the priority obvious.

## Label taxonomy

Every issue should have at least one `kind/` label and at least one `area/` label.
Priority and status labels are optional.

### `kind/` — what the issue is

| Label | When to apply |
|---|---|
| `kind/bug` | Something is broken or behaves incorrectly. The current behavior is wrong. |
| `kind/feature` | A new capability that does not exist today. Includes enhancements to existing features. |
| `kind/cleanup` | Code quality improvements that do not change behavior: refactors, naming, dead code removal, small inconsistencies. |
| `kind/docs` | Documentation only — missing, outdated, or incorrect docs. |
| `kind/design` | Design discussions, investigations, research, and open questions that must be resolved before implementation. Includes "Design discussion:", "Identify what X should be", "Measure/benchmark X", "Explore options". |

`kind/design` does not exist yet — create it first:
```bash
gh label create "kind/design" \
  --repo agent-substrate/substrate \
  --description "Design discussion, investigation, or research required before implementation" \
  --color "c5def5"
```

### `area/` — which part of the codebase

Apply all areas that are touched. Most issues need one or two; a few need more.

| Label | Scope |
|---|---|
| `area/api` | User-facing Substrate API (`pkg/proto/ateapipb/`, proto definitions, API semantics, versioning) |
| `area/api-machinery` | API server internals: RPC handlers, storage layer, syncer, controllers (`cmd/ateapi/`, `cmd/atecontroller/`) |
| `area/network` | Networking: atenet-router, Envoy, ext_proc, DNS, xDS, ingress (`cmd/atenet/`) |
| `area/node` | Node agent and worker lifecycle: atelet, sandbox launch, OCI, cgroups (`cmd/atelet/`) |
| `area/gvisor` | gVisor sandbox specifics: runsc integration, GPU in gVisor, gVisor OCI (`cmd/ateom-gvisor/`) |
| `area/microVM` | Micro-VM sandbox specifics: cloud-hypervisor, kata, snapshot/restore (`cmd/ateom-microvm/`) |
| `area/storage` | Snapshot storage, image cache, GCS/S3 backends, retention (`internal/imagecache/`, ActorSnapshot) |
| `area/scheduling` | WorkerPool sizing, actor placement, HPA, resource allocation |
| `area/observability` | Metrics, tracing, logging, OTLP, OTel SDK integration |
| `area/security` | Auth, mTLS, RBAC, threat detection, credential management, PodCertificate |
| `area/identity` | Actor identity, token issuance, JWT auth, ActorIdentity extension |
| `area/dev-infra` | CI/CD, GitHub Actions, presubmit checks, tooling, proto generation |
| `area/tests` | Test coverage, flaky tests, test infrastructure (not a bug in production code) |
| `area/cli` | `kubectl-ate` CLI plugin |
| `area/demos` | Demo applications in `demos/` |
| `area/benchmarking` | Performance benchmarks and capacity measurements (`benchmarking/`) |

`area/benchmarking` does not exist yet — create it first:
```bash
gh label create "area/benchmarking" \
  --repo agent-substrate/substrate \
  --description "Performance benchmarks and capacity measurement" \
  --color "f9d0c4"
```

### `prio/` — priority

Only apply when the issue body or a maintainer comment makes the priority evident.
Do not guess.

| Label | Meaning |
|---|---|
| `prio/P0` | Highest priority; required for next milestone or blocking ongoing work |
| `prio/P1` | Important but not blocking; should land in the near term |
| `prio/P2` | Real issue but not urgent; can wait |

### Status labels

Apply only when clearly appropriate. Never add `wontfix`, `duplicate`, or `invalid`
yourself — those are maintainer decisions.

| Label | When |
|---|---|
| `good first issue` | Well-scoped, self-contained, does not require deep system knowledge |
| `help wanted` | Maintainers actively want external contributions |

---

## Classification guide

Work through these questions in order:

**1. Is something broken?**
If the current behavior is incorrect, it is `kind/bug`. Security holes and data
loss are bugs, not features.

**2. Does it ask for new capability?**
If the system would need to do something it cannot do today, it is `kind/feature`.
Apply `kind/feature` even if the issue is framed as "support X" or "add Y".

**3. Is it a design question or investigation?**
If the issue is asking *how* to do something, comparing approaches, measuring
capacity, or researching unknowns before any code is written, it is `kind/design`.
Keywords: "Design discussion", "Investigate", "Explore options", "Identify what",
"Measure", "Research".

**4. Is it a code quality improvement with no behavior change?**
Refactoring, renaming, consolidating duplicated code, removing dead code → `kind/cleanup`.

**5. Is it documentation only?**
Docs fixes, missing API guide sections, README updates → `kind/docs`.

For `area/` labels, use the component the issue is *about*, not where the fix will
necessarily land. A bug in actor scheduling reported via the API is `area/api` +
`area/scheduling`, not `area/api-machinery`.

---

## Process

### Step 1 — Create missing labels (once)

Check whether `kind/design` and `area/benchmarking` exist before creating them:

```bash
gh label list --repo agent-substrate/substrate --json name \
  --jq '[.[].name] | contains(["kind/design"])'
```

Create each if not present using the `gh label create` commands above.

### Step 2 — Fetch unlabeled issues first

```bash
gh issue list --repo agent-substrate/substrate \
  --state open --limit 500 \
  --json number,title,labels,body \
  --jq '[.[] | select(.labels | length == 0)]'
```

### Step 3 — Classify and apply labels

For each issue, read the title and body, classify using the guide above, then apply:

```bash
gh issue edit <number> \
  --repo agent-substrate/substrate \
  --add-label "kind/bug,area/network"
```

Use `--add-label`, not `--label`, so you don't overwrite existing labels.

### Step 4 — Review already-labeled issues

After clearing the unlabeled backlog, scan labeled issues for obvious gaps:
- Issues with a `kind/` label but no `area/` label
- Issues with `area/` labels but no `kind/` label
- Issues that look like `kind/design` but are filed as `kind/feature`

Do not change labels that look reasonable even if you might have chosen differently.
Only correct clear mismatches (for example, a bug filed as `kind/feature`, or a
design discussion with no `kind/` label at all).

### Step 5 — Report

After triaging, output a table:

```
| Issue | Title (truncated) | Labels applied |
|---|---|---|
| #900 | Implement chain of authenticator... | kind/feature, area/identity, area/api-machinery |
```

List any issues you were uncertain about separately, with the question you could not
resolve from the text alone. Do not leave those unlabeled — apply your best guess and
flag it.

---

## Classification examples

These illustrate the taxonomy applied to real issues in this repo:

| Title pattern | `kind/` | `area/` |
|---|---|---|
| "X is broken / returns wrong value / panics" | bug | affected component |
| "Add support for X / Implement Y" | feature | affected component |
| "Design discussion: should X or Y" | design | affected component |
| "Identify what metrics to use for Z" | design | observability, scheduling |
| "Measure / benchmark X capacity" | design | benchmarking, affected component |
| "Consolidate / Reorganize / Rework X" | cleanup | affected component |
| "Update docs / Add doc for X" | docs | (omit area unless specific) |
| "E2E test flaky: TestFoo" | bug | tests, affected component |
| "Presubmit doesn't catch X" | bug | dev-infra |

---

## Notes

- An issue can have multiple `area/` labels. Apply all that fit.
- A single `kind/` label per issue is the norm. Applying two is unusual.
- Do not add `prio/` labels to issues that don't clearly warrant one. Unlabeled
  priority is better than a wrong priority label.
- Never remove a label a human applied. If you think it is wrong, note it in your
  report and let a maintainer decide.
