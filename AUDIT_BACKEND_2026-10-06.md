# Backend repair addendum, 6 October 2026

Base: `useteploy/teploy-arcade` at `2d0eef99f974a3ae89202d1bce004c6956a94707`.

Status: source repairs prepared for validation. None of these entries is runtime-verified, merged, or deployed. This addendum accompanies the existing `AUDIT_OPEN.md` register. Its entries should be reconciled into that register when integrating the separate backend and UI patches; no existing deferral is closed by this document.

- **A-01, High/P1: prepared for validation.** Container-only inspect recognizes the exact daemon missing-container response for the requested name only when the CLI exits 1, stdout contains no result, and the context remains active. Transport, permission, socket, executable, timeout, malformed, mixed and unrecognized errors stay Unknown. Start explicitly admits only Missing/Stopped to its cleanup path; every unknown enum value is refused. The named production inspector permits cancellation tests without replacing the classifier. The typed Engine API migration, CLI-version acceptance, and real fresh-start/stop-start/reboot tests remain open under R35/A24.
- **R43, Medium/P2: remaining snapshot slice repair prepared for validation.** Snapshot clones PendingRestart while holding s.mu, preserving nil versus empty slices. Added detached-reader, caller-mutation, actual SetResources append/sort, and concurrent JSON-encoding regressions. The previous metrics-sampler repair remains intact. Race-detector execution remains required.
- **A-06, Medium/P2: bounded arithmetic repair prepared for validation.** Linux memory usage now distinguishes field presence from zero availability, validates kB data, and subtracts before whole-MiB conversion. The 0–1023 kB availability range no longer becomes zero used memory. Invalid measurements remain the existing API's zero fallback: a nullable/freshness-aware host API and the R45 host-versus-cgroup scope problem are explicitly still open.
- **A-07, Low/P3: bounded arithmetic repair prepared for validation.** CPU accounting sums the first eight nonduplicated Linux counters; guest/guest_nice remain included through user/nice. Idle and iowait remain non-busy; steal remains busy/unavailable capacity, preserving prior policy. Malformed, overflowed and inconsistent samples/deltas are rejected. Counter reset/wrap cannot underflow busy usage. Invalid intervals still retain the previous displayed value without a freshness marker; that broader UI/API issue remains open.

Ten `TestBackend*` Go entrypoints were added. They use in-memory data, temp directories and a fake CLI that is the sole binary on PATH and refuses non-inspection commands. Go, gofmt, vet, race, live Docker/game, and browser checks have **not run**. No dependency changes or installation were made.

First validation commands in a dedicated, daemon-free Go 1.26 environment:

```sh
gofmt -w internal/arcade/runner.go internal/arcade/model.go internal/arcade/hostcap.go internal/arcade/hostcap_linux.go internal/arcade/manager.go internal/arcade/backend_*test.go
go test ./internal/arcade -run '^TestBackend' -count=1 -timeout=60s
go test -race ./internal/arcade -run '^TestBackend' -count=1 -timeout=60s
go vet ./...
```

Then run the full repository build/unit/race checks in that daemon-free environment. The existing untagged `TestAdoptedContainerExitIsNoticed` can create a real container when Docker is reachable, so the general suite is not automatically safe on a shared or production host. Run real lifecycle acceptance only on a separately authorized disposable Docker host.

**Unchanged high-risk open work:** A-02/R35 backup/restore unknown-state safety, A-03 player-list snapshot admission, durable recovery, and all other architectural deferrals in AUDIT_OPEN.md. This patch does not certify those paths.

Primary contract references:
- [Docker container-only inspect selection](https://raw.githubusercontent.com/docker/cli/master/cli/command/system/inspect.go)
- [Docker inspector exit status](https://raw.githubusercontent.com/docker/cli/master/cli/command/inspect/inspector.go)
- [Moby missing-container error](https://raw.githubusercontent.com/moby/moby/master/daemon/errors.go)
- [Linux guest accounting](https://raw.githubusercontent.com/torvalds/linux/master/kernel/sched/cputime.c)

## Validation update — 2026-10-06 landing

Executed in a dedicated daemon-free go1.26.6 environment at landing (see
AUDIT_OPEN.md's backend section for the reconciled register entries): gofmt,
build and vet clean; all ten `TestBackend*` entrypoints PASS plain and under
`-race`; full unit and race suites show only `TestAnImportedServerRunsItsOwnJar`
failing, which requires a live Docker daemon and reproduces identically on the
pristine base `2d0eef9` worktree — zero regressions introduced by this patch.
Still open: live Docker lifecycle, real browser checks, remote CI on the new
revision.
