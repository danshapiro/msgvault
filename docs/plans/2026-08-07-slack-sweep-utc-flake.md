# Slack Sweep UTC-Flake Fix — Implementation Plan

> **Revision note (Stage-2 load-bearing validation).** The first version of
> this document was a task-free HALT REPORT claiming the task's spec required
> halting if the flake proved to be a production bug. Validation **falsified
> that claim**: the governing spec (out-of-scope finding #1 from the
> `test-suite-speedups` run, plus the launch instruction) contains **no halt
> condition** — its directive is the test-only injected-clock fix. The
> underlying investigation was otherwise sound: every technical claim was
> independently re-verified (root cause, reproduction, reachability,
> coherent-clock behavior — see §2 and the assumption ledger at
> `.the-usual-logs/slack-sweep-utc-flake/load-bearing-ledger.md`). This
> revision converts the halt report into the implementation plan the spec
> actually asked for, while preserving the verified production-bug finding so
> it cannot be silently masked (§3 Task 3, §4).

**Goal:** Make `TestLimitOneSweepConverges` and
`TestSweepLimitOneConvergesPastTruncatedDayOverlap` (in
`internal/slack/importer_test.go`) deterministic regardless of where
wall-clock "now" sits within the UTC day, by driving every timestamp from a
single injected clock — without weakening the progress/error-visibility
invariants they pin. Test-only work: **no production `.go` file changes.**

**Non-goals:** fixing the production sweep-parking bug (owner's decision —
§4); changing any assertion's meaning; touching files outside
`internal/slack/*_test.go`.

## 1. Why the tests flake (verified)

The two tests mix clocks: fixture helpers (`tsBase` `importer_test.go:20`,
`tsFresh` `:30`, `tsAgo` `:2764`, inline reads at `:1261`/`:1351`) use real
`time.Now()`, while `imp.now` is also the real clock (the first `Import` of
every test runs unpinned — `testImporter`, `importer_test.go:90`). When the
real wall clock is near a UTC midnight, the persisted sweep floor lands in a
pathological band where production's `--limit 1` sweep genuinely makes no
durable progress, so the tests' (correct) assertions fail. Measured failure
windows at HEAD, empirically confirmed with a fully coherent injected clock
(evidence: `reports/V4.md`):

| Base (UTC) | 12:00:00 | 00:05:00 | 00:09:58 | 00:10:30 | 23:59:50 | 23:59:54 | 23:59:59 |
|---|---|---|---|---|---|---|---|
| Both tests | PASS | FAIL | FAIL | PASS | PASS | FAIL | FAIL |

Key verified consequence: **a coherent injected clock alone does not make the
tests pass near midnight** — production really fails there (§2). A
deterministic test-only fix must therefore anchor its default base outside
the pathological band, and the accidental bug-catching coverage must be
preserved explicitly (Task 3) rather than by luck.

## 2. The production finding this plan must not mask (verified)

Independently re-verified this stage against unmodified HEAD production code
(`reports/V2.md` static audit; `reports/V3.md` probe reproduction):

- The day walk's first day is a pure function of the persisted floor
  (`floor − sweepLagMargin` (10 min, `sweep.go:16`), day-truncated,
  `sweep.go:227`, `:235-236`, `:490-492`); `imp.now()` only bounds the far
  end (`:75`, `:237-238`). The budget charges one unit per searched day,
  check-before-charge (`:256-259`); the truncation exemption (`:241-255`) is
  the only free day; the watermark commits only via
  `tsLess(floor, min(nextBoundary, ceiling))` (`:230-234`).
- Therefore, when the floor sits in `[midnight, midnight + 10 min]` (user-tz
  midnight; ≈UTC in tests), a `--limit 1` sweep re-searches the previous,
  already-certified day forever: watermark frozen from +1m through +8d6h
  (E1/E2 probes). The parked state is reachable from ordinary mid-day
  operation — run 1 naturally commits the floor to exactly `midnight(D+1)`
  (E3), no wall-clock coincidence needed. A single `--limit 2` run escapes.
  The 7-day canonical thread audit eventually re-archives missed replies but
  never advances the sweep floor; the only other escape is a manual `--full`
  wholesale reset.
- `--limit` is a real production flag (`cmd/msgvault/cmd/sync_slack.go:119`)
  wired to this budget, and the tests pin the sweep's own documented contract
  ("repeated limited runs converge", `sweep.go:44`) — they are not
  over-assertions.

**Proposed issue (for the sweep owner to file):** `internal/slack`:
`--limit 1` sweep parks permanently once its watermark lands at/within 10
minutes after a (user-tz) midnight (reachable from ordinary operation;
surfaces as the known near-midnight test flake). Suggested fix shape
(**unvalidated suggestion**, mirrors the existing truncation exemption): do
not budget-charge a day whose `nextBoundary ≤ floor`. Ready-made regression
blueprints: `reports/production-gate-probe.md`, `reports/V3.md`.

## 3. Tasks

### Task 1 — Package test clock, coherently injected

**Files:** `internal/slack/clock_test.go` (new), `internal/slack/importer_test.go`.

1. Add a package-level test clock in `clock_test.go`:
   - `testClockBase`: defaults to **noon-of-today UTC** (outside every
     measured failure window); overridable via env
     `MSGVAULT_TEST_CLOCK_BASE` (RFC3339) for reproduction/debugging.
   - `testNow() time.Time`: `testClockBase + time.Since(processStart)` —
     advancing, deterministic base. (Blueprint validated end-to-end in
     `reports/V4.md`: this same shape produced the pass/fail matrix in §1.)
2. Install the clock as the importer's time source in `testImporter`
   (`importer_test.go:90`): `imp.now = testNow` — today the *first* `Import`
   of every test runs on the real clock; this closes that hole.
3. Replace **all** `time.Now()` reads in `importer_test.go` with `testNow()`
   (68 occurrences at HEAD, including the roots `tsBase` `:20`, `tsFresh`
   `:30`, `tsAgo` `:2764`, and inline reads at `:1261`, `:1351`).

**Verify:**
- `grep -c 'time\.Now(' internal/slack/importer_test.go` → `0`.
- `go test -tags "fts5 sqlite_vec" -count=1 ./internal/slack/` → all pass.
- `MSGVAULT_TEST_CLOCK_BASE=2026-08-07T12:00:00Z go test -tags "fts5 sqlite_vec" -count=3 -run '^(TestLimitOneSweepConverges|TestSweepLimitOneConvergesPastTruncatedDayOverlap)$' ./internal/slack/` → pass, 3/3.

### Task 2 — Midnight-crossing pinned subtests for the two named tests

**File:** `internal/slack/importer_test.go`.

1. Factor each of the two tests' bodies into a helper taking an explicit
   `base time.Time`. At helper entry, capture `anchor := time.Now()` and
   install `func() time.Time { return base.Add(time.Since(anchor)) }` as
   the importer's clock (the existing `Importer.now` seam, overriding the
   Task-1 default installed by `testImporter`). The parent test calls the
   helper with `base = testNow()` — same instantaneous value and advance
   rate as today's package clock, so parent behavior is preserved. The
   pinned subtests call it with the fixed bases in step 3. This
   **per-invocation anchoring** reproduces the regime the §1/V4 matrix
   measured (whole clock re-based to the pinned base, test starting at
   drift ≈ 0) on every run, regardless of `-count` iteration or position in
   the suite. Do NOT hand the subtests the package `testNow()` directly:
   its process-start anchor accumulates suite-runtime drift that would walk
   a pre-midnight base across a measured failure boundary (V4: the
   truncated-day test fails from a `23:59:54Z` base).
2. Make **every fixture timestamp** in the factored bodies derive from
   `base` — none from the package clock. Base-parameterize the fixture
   helper roots the two tests reach today: add `tsAt(base, ...)`,
   `tsFreshAt(base, ...)`, and `oldThreadWorkspaceAt(base)`
   (`oldThreadWorkspace`, `importer_test.go:542`, builds all its
   timestamps via `ts`), plus any other root a factored body turns out to
   reach (e.g., `tsAgo` `:2764`). Re-express the existing package-level
   helpers as thin wrappers over the `...At` variants anchored exactly
   where they anchor today (`ts` at `tsBase`; `tsFresh` at a live
   `testNow()` read) so the rest of the suite is unaffected. The factored
   bodies use only the `...At` variants, with fixture times computed as
   pure offsets from `base` at setup — leaving `imp.now()`'s own advance
   during the run as the only elapsed-time sensitivity. (Without this
   step, a "pinned" subtest would mix a fixed-date importer clock with
   noon-of-real-today fixtures — clock-incoherent, and not the regime §1
   verified.)
3. Add explicit-base subtests pinned at `2026-08-06T23:59:50Z` and
   `2026-08-06T00:10:30Z` — bases that genuinely straddle a UTC day
   boundary, selected from the §1/V4 matrix. The matrix was measured by
   re-basing the entire package clock, so it motivates the base choice;
   the **Verify gate below is the empirical confirmation for the subtest
   regime itself**. Same assertions as the parents — durable progress
   within 3 limit-1 runs / error visibility. Include a short comment table
   mapping bases → §1 matrix so future readers know why these bases were
   chosen. Margin note: V4 puts the truncated-day test's failure onset at
   a `23:59:54Z` base, so the pre-midnight subtest has ≈3–4 s of in-run
   drift budget; per-invocation anchoring plus offset-derived fixtures
   spends almost none of it. If the `-race` verify below ever approaches
   that budget, move that subtest's base earlier (e.g., `23:59:40Z`),
   re-verify 5/5, and record the measured margin in the comment table.

**Verify:**
`go test -tags "fts5 sqlite_vec" -race -count=5 -run '^(TestLimitOneSweepConverges|TestSweepLimitOneConvergesPastTruncatedDayOverlap)$' ./internal/slack/ -v`
→ parents + subtests pass, 5/5, at any wall-clock time (`-race`
deliberately stresses the drift margin); repeat without `-race` → 5/5.

### Task 3 — Skipped regression tests pinning the correct post-fix behavior

**File:** `internal/slack/sweep_parking_test.go` (new, test-only).

1. Port the three verified probe regimes (blueprints: `reports/V3.md`) as
   ordinary Go tests using the Task-1 clock and the existing `Importer.now`
   seam:
   - `TestSweepConvergesFloorJustAfterMidnight_KnownBug` (E1: floor 00:03Z),
   - `TestSweepConvergesFloorExactlyOnMidnight_KnownBug` (E2),
   - `TestSweepLimitOneConvergesFromDayBehindBacklog_KnownBug` (E3).
2. Each asserts **convergence** (the documented contract — the behavior that
   is *correct after* the production fix), and starts with
   `t.Skip("known production bug: --limit 1 sweep parks when floor lands within sweepLagMargin after midnight — see docs/plans/2026-08-07-slack-sweep-utc-flake.md §2; un-skip with the production fix")`.
3. These are the §2 finding's insurance: the coverage that accidentally
   caught the bug is now explicit, deterministic, and un-maskable — removing
   the skip is the one-line act that turns them into regression tests.

**Verify:** `go test -tags "fts5 sqlite_vec" -run 'KnownBug' ./internal/slack/ -v`
→ all three report `SKIP` with the message above; none fail.

### Task 4 — Whole-package determinism check (closes the survey's other flakes)

**Files:** none new (verification task; local test fixes only if needed).

1. Full suite at default anchor: `go test -tags "fts5 sqlite_vec" -count=3 ./internal/slack/` → green 3/3. This is expected to wholesale close the six
   other near-midnight-failing tests and nine latent ones found by the
   survey (`importer_test.go:3109`, `:560`, `:593`, `:2400`, `:2545`,
   `:2651`; latent: `:1062`, `:1117`, `:1229`, `:1539`, `:1715`, `:2032`,
   `:2165`, `:2985`, `:3039`) since they all root in the replaced
   `time.Now()` helpers (survey: `reports/time-now-pattern-survey.md`;
   **deferred assumption** — not re-validated this stage).
2. If any *other* test in the package objects to the pinned clock, fix it
   locally in that test (do not weaken the clock design); note it in the
   commit message.
3. Spot-check determinism where the suite used to flake:
   `MSGVAULT_TEST_CLOCK_BASE=2026-08-06T23:58:00Z go test -tags "fts5 sqlite_vec" -count=1 -run 'TestSweepFindsLateReplyToAncientThread|TestStandingLimitSweepsLateReplies' ./internal/slack/` → pass (bases outside each test's
   straddle windows per the survey; adjust base per the survey table if a
   chosen test's window differs).

**Verify:** steps 1 and 3 above; `git status --porcelain` shows only
`internal/slack/*_test.go` (+ this plan) modified.

## 4. What is deliberately NOT in this plan

- **No production change.** The §2 bug and its suggested fix belong to the
  sweep owner. This plan's Task 3 hands them ready regression tests.
- **No assertion weakening.** Every existing assertion keeps its meaning;
  new subtests only add pinned coverage.
- **No un-skipping of Task-3 tests** — they document a real, currently
  failing behavior and must stay skipped until production is fixed.

## 5. Evidence artifacts

The investigation's working artifacts (assumption ledger: 13 verified,
1 falsified; independent reproductions E1/E2/E3; coherent-clock matrix;
time-source survey; production gate probe) live in a local investigation
workspace outside this repository. The durable evidence is in this repo:
the pinned midnight-straddling subtests and the skipped `KnownBug`
regression tests in `internal/slack/`.
