# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 3 P2 (3 total)

## useteploy__teploy-arcade-01 - P2 - Open

**Preserve per-room delivery order across concurrent publishers**

- Kind: Confirmed from source
- Evidence: Publish assigns sequence numbers under r.mu, copies the subscriber set, then unlocks before JSON encoding and sending. Another publisher can assign and deliver sequence N+1 before the first delivers N.
- Impact: A subscriber can observe out-of-order lines despite the ordered replay buffer; clients using a last-seen watermark may misclassify the late line as a duplicate or a gap. Frontend handling was not inspected.
- Proposed fix: Serialize per-room enqueue/delivery ordering with sequence assignment, or use a single ordered dispatcher; keep slow consumers nonblocking while preserving order for each viewer.
- Acceptance test: Pause one publisher after assigning sequence 1, allow a second publisher to proceed, and assert the subscriber still receives sequence 1 before 2.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)

## useteploy__teploy-arcade-02 - P2 - Open

**Enforce deleted-room tombstones on every entry path**

- Kind: Confirmed from source
- Evidence: DropRoom returns without recording a tombstone if no room exists. A later Publish can then create the deleted room. When a dead room does exist, Join does not check r.dead and adds a new open connection after the deletion cleanup has run.
- Impact: Late work can recreate a supposedly deleted room, or a racing/new viewer can attach to a room that will never publish and no longer has a deletion pass to close it.
- Proposed fix: Record deletion even for absent rooms, reject/close joins to dead rooms, and use generation-aware room identities when recreation of a server ID is intentional.
- Acceptance test: Test DropRoom before the first Publish, Join immediately after DropRoom, and concurrent Join/DropRoom; no deleted room may gain live viewers or buffered lines.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)

## useteploy__teploy-arcade-03 - P2 - Open improvement

**Make panel startup and shutdown own all spawned background workers**

- Kind: Improvement
- Evidence: Run launches metrics, sampling, guard, player-sync, scheduler and session-reaper goroutines before ListenAndServe, with no caller-visible cancellation/shutdown handle in this entry point.
- Impact: A bind failure or reuse of Run in tests/embedding can leave workers behind while the process remains alive; graceful panel restart policy is also not expressed here.
- Proposed fix: Accept an application context, bind the listener before starting long-lived workers, stop/join workers on errors and shutdown, and document which game containers intentionally survive the panel.
- Acceptance test: Force a port-bind failure and cancel a running panel; assert all panel-owned workers exit while the explicit game-runtime continuity policy is respected.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)

