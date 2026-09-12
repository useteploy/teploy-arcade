# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 1 P2 improvement (1 total)

## useteploy__teploy-arcade-03 - P2 - Open improvement

**Make panel startup and shutdown own all spawned background workers**

- Kind: Improvement
- Evidence: Run launches metrics, sampling, guard, player-sync, scheduler and session-reaper goroutines before ListenAndServe, with no caller-visible cancellation/shutdown handle in this entry point.
- Impact: A bind failure or reuse of Run in tests/embedding can leave workers behind while the process remains alive; graceful panel restart policy is also not expressed here.
- Proposed fix: Accept an application context, bind the listener before starting long-lived workers, stop/join workers on errors and shutdown, and document which game containers intentionally survive the panel.
- Acceptance test: Force a port-bind failure and cancel a running panel; assert all panel-owned workers exit while the explicit game-runtime continuity policy is respected.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)


## Resolution log (2026-09-12)

- teploy-arcade-01, -02: FIXED - see audit commit (fan-out moved under r.mu; trySend stays nonblocking; DropRoom records absent-room tombstones; Join refuses dead rooms with tests).
- teploy-arcade-03: DEFERRED (design) - Run()'s worker-lifecycle ownership (context, pre-bind listener, join-on-shutdown) is an entry-point API change; decide the embedding/test story first.
