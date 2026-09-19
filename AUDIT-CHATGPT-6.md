# Teploy Arcade — Code Audit and Remediation Report

**Repository:** `useteploy/teploy-arcade`  
**Reviewed commit:** `dcfe671f707bb36fe541690fd27815b37c6435ae`  
**Audit date:** September 18, 2026  
**Deliverable:** One Markdown file containing findings, suggested implementations, regression cases, source references, and validation limits.

## Executive summary

This report records **43 findings: 2 Critical, 20 High, 19 Medium, and 2 Low**. Findings include confirmed code defects, concurrency/failure scenarios supported by the inspected implementation, and explicitly identified hardening or product-contract gaps. These counts are not a count of independently verified exploits.

The most urgent issues are the operational authorization bypass on an unclaimed instance and restore recovery that can delete original files after a crash during evacuation. Other important clusters involve backup integrity, persistence outcomes, port reservations, concurrency, Docker supervision, and plugin publication. The respective findings below give their preconditions, evidence, and remediation. [AUTH] [APP] [BACKUP] [MANAGER] [PLUGINS]

**Scope and assurance:** Actual files were retrieved from GitHub at the commit above and inspected, including the authentication, HTTP/API, backup/restore, file, scheduler, MCP integration, plugin, runner/manager, startup, frontend, and workflow areas cited below. The audit does **not** certify that every possible bug in the repository has been found. This is a source-led audit with targeted isolated execution, not a completed production penetration test or a full repository build/test run.

The review concentrated on substantial relevant sections of the large manager, runner, API, file, backup, and frontend files. Authentication, app startup, scheduler, plugin management, MCP integration, the main entry point, and the listed workflows were inspected through their fetched source. Other modules—including the full import/clone implementation, all player/host/template logic, every frontend view/style, the internal MCP transport/dispatcher, and the complete existing test suite—were not comprehensively reviewed. Cross-cutting fixes explicitly name additional call sites that need integration review.

The repository contains previous audit documents. They were used for context, not treated as proof that code was correct or as findings to copy blindly. In particular, this report does not re-report the corrected SHA-256 MCP token hashing, the basic `os.Root` file sandbox, randomized token entropy handling, `saveMu` serialization, role-gated ordinary routes after setup, or the existing Go race-test CI as if those protections were absent. New defects in adjacent paths or incomplete fixes are identified separately. [AUTH] [FILES] [MCP] [MANAGER] [CI] [AUDITOPEN]

### How to use the implementations

Each finding includes code plus a regression criterion. Some blocks are complete replacement helpers; others are explicitly marked integration fragments or conservative alternatives. **Do not concatenate every code block into one Go file.** Add the indicated imports, update signatures/call sites, and keep one definition of shared types/helpers. The code targets the repository's Go 1.26 API level unless identified as an isolated probe. The report is not a pre-applied or fully compiled patch set.

Shared implementation R1 includes a recovery core that passed eight isolated tests. R2 provides the reservation-lease fix, and R3 provides a strict JSON decoder. Apart from the documented isolated checks, proposed changes require compilation, integration review, and regression testing in the repository. Newly introduced integration symbols are explained where used rather than presented as existing APIs.

### Severity scale

**Critical** means reachable administrative bypass or an automatic recovery path with substantial original-data loss under the stated conditions. **High** means serious integrity, confidentiality, supervision, or availability risk requiring prompt correction. **Medium** means meaningful correctness, security-hardening, reliability, or operator-safety impact with narrower conditions. **Low** means a bounded correctness or engineering-control gap. Severity reflects this panel's privileged operational context, not a calculated CVSS score.

## Findings index

| ID | Severity | Finding |
|---|---|---|
| [A01](#a01) | Critical | First-run setup protects account creation, but leaves operational routes unauthenticated |
| [A02](#a02) | Critical | Restore crash recovery deletes original entries that were never moved aside |
| [A03](#a03) | High | Restore rollback silently fails and then deletes the only remaining recovery copy |
| [A04](#a04) | High | Archive extraction can accept an unverified gzip stream and ignores output-close failures |
| [A05](#a05) | High | A server in starting state can be backed up without quiescing its writes |
| [A06](#a06) | High | Live-backup success is based on command delivery, not verified save state, and resume failure is misreported |
| [A07](#a07) | High | Restore disk admission uses an unreliable gzip-size estimate rather than a real budget |
| [A08](#a08) | High | The archiver is not root-confined and can follow a raced symlink or block on a special file |
| [A09](#a09) | High | Authentication and other state loaders accept corrupt, partial, or null records |
| [A10](#a10) | High | Failed persistence still changes credentials, token revocation, and scheduled work in memory |
| [A11](#a11) | High | Public login work and audit writes have no rate, concurrency, or field-size budget |
| [A12](#a12) | High | Revoked sessions retain read access through existing event and console streams |
| [A13](#a13) | Medium | Session cookies are not Secure and browser mutation requests have no explicit origin protection |
| [A14](#a14) | High | Streaming deadline removal reopens slow-body and stalled-writer resource exhaustion |
| [A15](#a15) | High | Shared filesystem locks do not serialize configuration transactions against one another |
| [A16](#a16) | High | Properties-file identity and parsing disagree across validation, publication, and reload |
| [A17](#a17) | Medium | File-read and directory-list limits are enforced after unbounded work |
| [A18](#a18) | Medium | Atomic replacement does not preserve all file metadata or complete directory durability |
| [A19](#a19) | High | Port reservations leak after creation and unrelated operations share a reservation identity |
| [A20](#a20) | High | Several live Server reads bypass the mutex that protects their writes |
| [A21](#a21) | High | Server deletion ignores persistence and filesystem failures after removing worlds and backups |
| [A22](#a22) | Medium | Queued operations can recreate a server directory after the server was deleted |
| [A23](#a23) | High | A failed Docker kill cancels supervision of a container that may still be running |
| [A24](#a24) | High | Docker transport errors are confused with process death, and exit 137 is always labeled OOM |
| [A25](#a25) | Medium | Docker control calls and captured output are not consistently bounded or cancellable |
| [A26](#a26) | Medium | Oversized or interrupted Docker log output silently ends the console reader |
| [A27](#a27) | High | MCP console restrictions are a bypassable first-word denylist, not a capability boundary |
| [A28](#a28) | Medium | Duplicate MCP token names make revocation ambiguous and incomplete |
| [A29](#a29) | Medium | Task creation uses collision-prone IDs, borrows mutable input, and accepts server-owned metadata |
| [A30](#a30) | Medium | Task mutation routes ignore the server ID in their URL and can misattribute audit records |
| [A31](#a31) | Medium | Scheduler civil-time calculations disagree with actual firing at DST and day boundaries |
| [A32](#a32) | Medium | Scheduled executions have no application cancellation or global work budget |
| [A33](#a33) | Medium | Run does not own worker shutdown, starts work before binding, and formats IPv6 addresses incorrectly |
| [A34](#a34) | Medium | Third-party player-head requests are enabled by default despite an opt-in privacy claim |
| [A35](#a35) | Medium | The delete confirmation understates destruction of server data and backups |
| [A36](#a36) | High | Concurrent plugin installation or toggling can overwrite a file despite the no-overwrite promise |
| [A37](#a37) | Medium | Plugin download validation accepts a four-byte signature rather than an intact executable archive |
| [A38](#a38) | Medium | Plugin installation can succeed after the HTTP response has already timed out |
| [A39](#a39) | Medium | Release publication is not gated by the repository tests |
| [A40](#a40) | Low | Build reproducibility and regression coverage do not yet match the failure modes of this service |
| [A41](#a41) | Medium | Failed archive assembly leaves partial files behind and reuses a predictable temporary name |
| [A42](#a42) | Medium | Creation validates supplied resource values before defaults, and silently converts unknown runtimes to simulator |
| [A43](#a43) | Low | NextFreePort can recommend an occupied or out-of-range port and ignores candidate geometry |

## Detailed findings

<a id="a01"></a>

### A01 — Critical — First-run setup protects account creation, but leaves operational routes unauthenticated

**Evidence:** `Auth.gate` immediately calls the protected handler whenever `Enabled()` is false. `BeginSetup` generates a token but does not enable an operational setup gate. Consequently, an unclaimed panel permits routes such as server creation/deletion, file access, and MCP-token issuance without that token. The token check exists only in `/api/setup`. `Run` has no intervening middleware that closes these routes. The container defaults to `0.0.0.0`. [AUTH] [APP] [API] [APIEXT] [MCP] [DOCKER]

**Impact and precondition:** A reachable, unprovisioned instance gives an unauthenticated caller administrative application capabilities. A token issued through the unprotected MCP-token route can remain valid after the legitimate administrator completes setup. This is critical for a network-exposed instance with the Docker socket mounted. The standalone binary's loopback default reduces exposure; it does not correct the authorization rule. This is distinct from an explicitly requested `--no-auth` development session.

**Fix:** Model *setup required*, *authenticated*, and *explicit development bypass* separately. Only the explicit bypass may skip authorization. Keep health, login-status, and token-gated setup available during setup. Apply the setup gate to MCP dispatch too, so pre-existing tokens cannot bypass an unclaimed state.

```go
// auth.go. Replace gate's initial !Enabled() fast path with this check.
func (a *Auth) accessMode() (bypass, setupRequired bool) {
    a.mu.RLock()
    defer a.mu.RUnlock()
    return a.forced, !a.forced && len(a.users) == 0
}

// At the beginning of Auth.gate's returned handler:
bypass, setupRequired := a.accessMode()
if bypass {
    next(w, r)
    return
}
if setupRequired {
    writeJSON(w, http.StatusServiceUnavailable, map[string]string{
        "error": "initial administrator setup is required",
    })
    return
}
// Continue with the existing session, role, and MustChange checks.
```

Use the same `setupRequired` check before `mcp.Handler.ServeHTTP`; do not add cookie authentication to legitimate bearer-token calls. Reject non-loopback `--no-auth` unless an independently named, conspicuous unsafe override is supplied.

**Regression:** With a fresh data directory and a valid bootstrap token generated, anonymous requests to protected routes must fail *before invoking their handlers*. Test both HTTP and WebSocket handshakes; verify `/api/mcp-tokens` cannot mint a credential. Test explicit development bypass separately.

<a id="a02"></a>

### A02 — Critical — Restore crash recovery deletes original entries that were never moved aside

**Evidence:** `RestoreBackup` moves live top-level entries into `staging/old` one at a time. `recoverInterruptedRestores` interprets *any nonempty* `old/` as proof that *every* original entry was moved, then removes every live entry except the staging directory. That implication is false during the evacuation loop. [BACKUP] [MANAGER]

**Reproduction:** Start with `a.txt` and `b.txt`; move only `a.txt` into `old/`; simulate process death. Recovery deletes `b.txt`, then restores only `a.txt`. An isolated filesystem probe reproduced this loss. The earlier audit's “all live entries are new” assumption is the defect, not a missing archive-extraction check.

**Fix:** Stop deriving transaction phase from directory contents. Before moving anything, durably record a journal containing the original names, replacement names, transaction identity, and phase. Distinguish evacuation from installation and distinguish rollback removal from rollback restoration. Store an authoritative transaction record outside the game-writable tree; treat unfamiliar `.arcade-restore-*` directories as untrusted leftovers, not instructions to delete data.

```go
// Core phase model; see Implementation R1 for the recovery algorithm.
type restorePhase string
const (
    phaseEvacuating restorePhase = "evacuating"
    phaseInstalling restorePhase = "installing"
    phaseRemoveNew  restorePhase = "rollback-remove-new"
    phaseRestoreOld restorePhase = "rollback-restore-old"
    phaseCommitted restorePhase = "committed"
    phaseRolledBack restorePhase = "rolled-back"
)

type restoreJournal struct {
    Version  int          `json:"version"`
    ServerID string       `json:"server_id"`
    TxID     string       `json:"tx_id"`
    Phase    restorePhase `json:"phase"`
    OldNames []string     `json:"old_names"`
    NewNames []string     `json:"new_names"`
}
```

Only switch to `installing` after all original entries have moved and both directories have been synced. On recovery from `evacuating`, restore only entries actually present in `old/`; never delete the remaining originals. The full recovery core in R1 also handles a second crash during rollback.

**Regression:** Inject process termination before and after **every rename and phase write**, including the first original move. Verify byte-for-byte preservation of all originals, empty original directories, colliding top-level names, and interrupted rollback. A single “crash during installation” test is insufficient.

<a id="a03"></a>

### A03 — High — Restore rollback silently fails and then deletes the only remaining recovery copy

**Evidence:** `restoreHeld` discards every rename error; `rollbackRestore` discards removal errors. `RestoreBackup` unconditionally defers `os.RemoveAll(staging)`, even when rollback cannot restore an entry. Boot recovery likewise removes staging after best-effort recovery. Messages claim the old world was restored regardless of these outcomes. `.committed` creation is also only logged on failure and is not durably synced. [BACKUP] [MANAGER]

**Impact:** A permissions error, filesystem failure, or destination collision can leave original data in `old/`, after which cleanup destroys it. A crash can also roll back a restore that was reported successful because commit-marker durability was not established.

**Fix:** Recovery errors must be first-class results. Preserve the transaction directory and block startup for the affected server until recovery completes. Mark a transaction committed only after installed data, ownership changes, directory entries, and the journal are durable. Do not claim that `fsync` of the archive alone makes the directory replacement durable.

```go
// Replace unconditional staging cleanup with outcome-driven cleanup.
cleanable := false
// This defer may delete only an already committed or fully rolled-back txn.
defer func() {
    if cleanable {
        if err := os.RemoveAll(staging); err != nil {
            log.Printf("restore cleanup retained at %s: %v", staging, err)
        }
    }
}()

// On an installation error:
if rollbackErr := recoverRestore(store, root, stagingName, &journal); rollbackErr != nil {
    return errors.Join(installErr,
        fmt.Errorf("rollback incomplete; recovery data retained at %s: %w",
            staging, rollbackErr))
}
cleanable = true // Only after the journal says rolled-back.
return installErr
```

The names in this integration excerpt correspond to the interfaces in R1. It replaces, rather than supplements, the current unconditional defer. An incomplete rollback must not proceed to automatic game-container resume.

**Regression:** Inject failures into removal, rollback rename, owner repair, journal sync, and staging cleanup. Assert that an error never deletes the last recoverable original and that the next restart resumes rollback idempotently.

**Model recovery must participate too:** `reloadProps` can save the replacement port/properties before the commit marker exists. Boot recovery restores files but does not roll that saved model back. Journal before/after model snapshots and binding leases, reconcile the appropriate snapshot before any game resumes, and retain recovery records until both filesystem and model repair have succeeded. R1 below deliberately implements the filesystem recovery core, not a claim of atomicity across `servers.json` and world data. [BACKUP] [MANAGER]

<a id="a04"></a>

### A04 — High — Archive extraction can accept an unverified gzip stream and ignores output-close failures

**Evidence:** `untarGzLimited` returns success as soon as `tar.Reader.Next()` returns `io.EOF`. It does not consume the gzip reader to its own EOF. `gzip.Reader.Close` does not verify the checksum. Extracted-file `Close` errors are discarded, and the number of copied bytes is not explicitly checked against the header. [BACKUP] [GZIPDOC]

**Validation:** An isolated Go probe created a tar.gz with a corrupt gzip trailer. The tar reader reported EOF; draining the gzip reader subsequently reported `gzip.ErrChecksum`. This demonstrates the missing validation step without accessing a live server.

**Fix implementation:**

```go
// In untarGzLimited, replace the tar EOF success branch.
if err == io.EOF {
    // Accept only a small, bounded tail after tar EOF; drain through gzip EOF
    // to verify its checksum. Do not introduce an unlimited decompression path.
    const maxTail = int64(1 << 20)
    n, drainErr := io.Copy(io.Discard, io.LimitReader(gz, maxTail+1))
    if drainErr != nil {
        return fmt.Errorf("gzip integrity check: %w", drainErr)
    }
    if n > maxTail {
        return fmt.Errorf("excess data after the tar end marker")
    }
    return gz.Close()
}

// Replace the regular-file copy/close block.
n, copyErr := io.CopyN(out, tr, hdr.Size)
if copyErr == nil && n != hdr.Size {
    copyErr = io.ErrUnexpectedEOF
}
var syncErr error
if copyErr == nil {
    syncErr = out.Sync()
}
closeErr := out.Close()
if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
    return fmt.Errorf("extract %q: %w", hdr.Name, err)
}
total += n
```

Validate directory-entry durability as part of R1, and reject unsupported archive types rather than silently implying an exact restore. Consider explicit rejection of additional gzip members and unexpected nonzero tar-tail data for a stricter format contract.

**Regression:** Corrupt CRC and ISIZE, truncate the compressed stream, fail file close, and provide an overlong trailing stream. None may reach the live-tree swap.

<a id="a05"></a>

### A05 — High — A server in starting state can be backed up without quiescing its writes

**Evidence:** `quiesceForBackup` treats every state except `running` as safe and returns a no-op. `Start` releases `fsMu` after launching/attaching the container, while the game can remain `starting` and generate or modify its world. `CreateBackup` can then take the filesystem lock and archive that actively changing tree without pausing the game. [BACKUP] [MANAGER] [RUNNER]

**Fix:** Explicitly classify states, rather than equating “not ready” with “not writing.” A backup should reject transitional states. Even `failed` is not proof of process death when supervision has failed; verify container absence before an offline snapshot. Apply the same invariant to every consumer of `quiesceForBackup`.

```go
func (m *Manager) validateSnapshotState(s *Server) error {
    switch s.State() {
    case StatusRunning:
        return nil // Requires an explicit, verified live-snapshot strategy.
    case StatusStopped, StatusFailed:
        if s.Runtime == RuntimeDocker {
            ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
            defer cancel()
            state, err := inspectState(ctx, containerPrefix+"-"+s.ID) // A24
            if err != nil { return fmt.Errorf("container state is unknown: %w", err) }
            if state.Running { return fmt.Errorf("container is still running") }
        }
        return nil
    default:
        return fmt.Errorf("wait until startup or shutdown finishes before backup")
    }
}

// Invoke while holding s.fsMu exclusively, before measuring or archiving.
if err := m.validateSnapshotState(s); err != nil {
    return nil, err
}
```

This uses the error-aware inspection helper in A24. Add a structured known-missing result for containers that have never existed; do not turn every inspection failure into that result. A failed Docker query is not proof that a container is stopped. Derive the context from the application/operation context when threading cancellation through the manager.

**Regression:** Hold a fake runner in `starting` while it writes, then request a backup. The request must fail without creating an archive. Test unknown Docker state separately.

<a id="a06"></a>

### A06 — High — Live-backup success is based on command delivery, not verified save state, and resume failure is misreported

**Evidence:** `dockerRunner.Send` discards a successful command's response text. `quiesceForBackup` accepts process exit status plus a fixed sleep as confirmation of `save-off` and `save-all flush`. Its resume callback logs failure but cannot return one; the enclosing defer then always emits “world saves resumed.” [BACKUP] [RUNNER]

**Impact:** Delivery success is not a general proof that a customized server implemented the requested save operation. More concretely, a failed `save-on` still produces a success message and successful backup response while automatic saves may remain disabled.

**Fix:** Prefer fail-closed offline snapshots until a template has an explicitly implemented acknowledgement contract. This conservative implementation removes the unsafe inference immediately:

```go
func (m *Manager) quiesceForBackup(s *Server) (func(), error) {
    if err := m.validateSnapshotState(s); err != nil { return nil, err }
    switch s.State() {
    case StatusStopped, StatusFailed:
        return func() {}, nil
    default:
        return nil, fmt.Errorf("stop the server before taking a verified snapshot")
    }
}
```

Alternatively, a retained live implementation should change the resume signature and expose `Quiesce(ctx) (ResumeFunc, error)` on a template-specific snapshot adapter; retain and verify the RCON reply and return resume errors. Report `archive_created=true, saves_resumed=false` when the archive exists but restoration of saving fails. Do not retry an entire backup merely because resuming failed.

```go
// Resume must execute on both success and failure, but success messages must
// reflect its result. This uses a named result error in CreateBackup.
defer func() {
    if e := resume(); e != nil {
        retErr = errors.Join(retErr, fmt.Errorf("automatic saves not resumed: %w", e))
        m.panelLine(s, "error", "Automatic saves remain disabled; operator action required.")
    }
}()
```

**Regression:** Simulate a successfully delivered but rejected save command and a failed resume command. Neither may be represented as a verified live snapshot with saves resumed.

<a id="a07"></a>

### A07 — High — Restore disk admission uses an unreliable gzip-size estimate rather than a real budget

**Evidence:** `uncompressedSize` reads the 32-bit gzip ISIZE trailer and assumes wraparound only when ISIZE is no larger than compressed size. Wraparound can also leave a value larger than compressed size. The alternative `compressed * 4` is not an upper bound. Extraction allows up to 64 GiB regardless of current free space, and per-server locks do not reserve shared capacity across servers. [BACKUP]

**Example:** An archive expanding to 5 GiB has an ISIZE near 1 GiB; when its compressed size is below that, the check understates the need by roughly 4 GiB. This is arithmetic, not a claim that such an archive was generated during the audit.

**Fix:** Read validated tar headers to calculate a bounded extraction total, reject arithmetic overflow, and reserve bytes/inodes on the destination filesystem before writing. Continue monitoring free space because games and other processes can consume it after admission. A count-only preflight must itself bound compressed/decompressed work and verify gzip integrity.

```go
func checkedTotal(total, next, maximum int64) (int64, error) {
    if next < 0 || total < 0 || next > maximum-total {
        return 0, fmt.Errorf("archive exceeds extraction budget")
    }
    return total + next, nil
}

// Before extracting each entry, using a budget reserved for this filesystem:
if hdr.Size > reservation.Remaining() {
    return fmt.Errorf("archive exceeds its reserved disk budget")
}
free, err := diskFree(destination)
if err != nil {
    return fmt.Errorf("cannot verify destination capacity: %w", err)
}
if free < importFreeMargin || hdr.Size > free-importFreeMargin {
    return fmt.Errorf("insufficient safe free space for the next entry")
}
```

`reservation` is a new shared capacity-manager interface, not an existing repository symbol. Its admission/release operations must use a filesystem identity (not server ID), a mutex, and deferred release. A periodic free-space check alone is not a hard quota; use filesystem quotas where strict isolation is required.

**Regression:** Test ISIZE wraparound, two concurrent restores on one volume, inode exhaustion, and an externally shrinking free-space budget. Keep the original world intact on every refusal.

<a id="a08"></a>

### A08 — High — The archiver is not root-confined and can follow a raced symlink or block on a special file

**Evidence:** The file API uses `os.Root`, but `tarGz` uses `filepath.Walk`, inspects an entry, and later calls `os.Open(path)`. A game/plugin process is not controlled by `s.fsMu`; it can replace the entry between those operations. The archiver also opens non-directory, non-symlink special files without requiring a regular file. [BACKUP] [FILES]

**Impact:** A writable server tree can cause the privileged archiver to read outside that tree through a replacement symlink, or hang opening a FIFO. The `fsMu` fix protects cooperating panel operations, not arbitrary filesystem writers. Exploitability depends on the game/plugin account's ability to mutate entries during archiving.

**Fix:** Walk relative to a held root handle; reopen every file through that root; check the descriptor type; skip or explicitly reject links and special files. Use the descriptor's metadata for the archive header, not stale walk metadata. On the supported Unix platforms, nonblocking open avoids hanging before the type check:

```go
func openRegularIn(root *os.Root, name string) (*os.File, os.FileInfo, error) {
    // Linux/Darwin implementation; keep platform-specific flags in build-tagged files.
    f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
    if err != nil { return nil, nil, err }
    st, err := f.Stat()
    if err != nil { f.Close(); return nil, nil, err }
    if !st.Mode().IsRegular() {
        f.Close()
        return nil, nil, fmt.Errorf("%q is not a regular file", name)
    }
    return f, st, nil
}
```

Use `os.Root` throughout traversal as well; merely resolving a path once is not the fix. This prevents path escape, not application-level world-write inconsistency—that remains A05/A06.

**Regression:** Race an entry with a symlink to an outside sentinel while archiving. The sentinel's contents must never enter the archive. FIFO/device entries must fail promptly rather than stall the request.

<a id="a09"></a>

### A09 — High — Authentication and other state loaders accept corrupt, partial, or null records

**Evidence:** `Auth.Load` ignores user-file read failures, quarantines malformed JSON but continues using the possibly partially decoded slice, and dereferences `*User` elements without nil checks. `Manager.Load` and the scheduler also accept pointer-element lists without validating null records and uniqueness. An isolated JSON probe confirmed that `[null]` decodes successfully into a slice containing a nil pointer. [AUTH] [MANAGER] [SCHED]

**Impact:** A permission/read error can be mistaken for a new installation; malformed state can expose the first-run authorization defect; syntactically valid null records can panic at startup or kill the scheduler loop. Duplicate IDs/names and invalid roles can create ambiguous or unusable state.

**Fix:** Distinguish absent from unreadable, decode into a temporary value, validate every record and cross-record invariant, then publish the whole validated snapshot. Never recover authentication by silently opening access. Quarantining a copy for diagnostics is compatible with refusing startup.

```go
func loadUsersFile(filename string) (map[string]*User, error) {
    b, err := os.ReadFile(filename)
    if errors.Is(err, os.ErrNotExist) { return map[string]*User{}, nil }
    if err != nil { return nil, fmt.Errorf("read users: %w", err) }
    if strings.TrimSpace(string(b)) == "null" { return nil, fmt.Errorf("null users store") }
    var list []*User
    if err := json.Unmarshal(b, &list); err != nil {
        return nil, fmt.Errorf("invalid users file; repair required: %w", err)
    }
    out := make(map[string]*User, len(list))
    admins := 0
    for i, u := range list {
        if u == nil { return nil, fmt.Errorf("null user at index %d", i) }
        key := strings.ToLower(strings.TrimSpace(u.Name))
        if key == "" || roleRank[u.Role] == 0 {
            return nil, fmt.Errorf("invalid user at index %d", i)
        }
        if _, exists := out[key]; exists { return nil, fmt.Errorf("duplicate user %q", key) }
        salt, e1 := hex.DecodeString(u.Salt)
        hash, e2 := hex.DecodeString(u.Hash)
        if e1 != nil || e2 != nil || len(salt) != 16 || len(hash) != 32 {
            return nil, fmt.Errorf("invalid credentials for %q", u.Name)
        }
        cp := *u
        out[key] = &cp
        if u.Role == RoleAdmin { admins++ }
    }
    if len(out) > 0 && admins == 0 { return nil, fmt.Errorf("no administrator in users file") }
    return out, nil
}
```

Preserve legacy username normalization rules during migration; do not silently rename accounts referenced by sessions or audit entries. Add equivalent validation for server IDs, template/runtime values, task IDs, server references, and token records. The credential lengths above match this snapshot's legacy format; extend validation when adopting versioned hashes.

**Regression:** ENOENT, EACCES, truncated JSON, a partial decode error, `[null]`, duplicate case-insensitive names, invalid hashes, and a store with no admin must have explicit outcomes.

<a id="a10"></a>

### A10 — High — Failed persistence still changes credentials, token revocation, and scheduled work in memory

**Evidence:** `createLocked`, `SetPassword`, and `DeleteUser` mutate users/sessions before `saveUsers` returns. MCP `Issue`/`Revoke` and scheduler `Add`/`Update`/`Delete` have the same shape. Scheduler validation failure rolls back, but save failure does not. Resource updates and reordering also return a persistence error after mutating live state. [AUTH] [MCP] [SCHED] [MANAGER]

**Impact:** An API failure is not a failed operation. A rejected password change can invalidate the old password for the current process but disappear after restart; failed user/token revocation can be undone by restarting; an allegedly failed task creation can still run. Retrying a failed request can create duplicates.

**Fix:** Build a prospective snapshot, persist it under the writer transaction lock, and only then publish it in memory and revoke sessions. Do not re-use slice backing arrays or mutate pointed-to records while constructing the candidate.

```go
// auth.go: called with a.mu held, after hashes were computed outside the lock.
func (a *Auth) commitUsersLocked(next map[string]*User) error {
    list := make([]*User, 0, len(next))
    for _, u := range next { list = append(list, u) }
    sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
    b, err := json.MarshalIndent(list, "", "  ")
    if err != nil { return err }
    if err := writeFileAtomic(a.usersPath(), b, 0o600); err != nil { return err }
    a.users = next
    a.enabled = !a.forced && len(next) > 0
    return nil
}

func cloneUsers(src map[string]*User) map[string]*User {
    dst := make(map[string]*User, len(src))
    for k, u := range src { cp := *u; dst[k] = &cp }
    return dst
}

// SetPassword commit section, after revalidating the hash snapshot:
nextUsers := cloneUsers(a.users)
nextUsers[key].Salt, nextUsers[key].Hash = salt, newHash
nextUsers[key].MustChange = byAdmin
if err := a.commitUsersLocked(nextUsers); err != nil { return err }
for token, sess := range a.sessions {
    if strings.EqualFold(sess.User, name) && token != keepToken {
        delete(a.sessions, token)
    }
}
return nil
```

Use the same persist-then-publish transaction for MCP token slices and task slices. `LastUse` telemetry may remain best-effort; issuance and revocation must not. For server state plus game files, use a journal or explicit “applied but not durable” result; a map-copy alone cannot make a multi-file operation atomic.

**Regression:** Inject write, sync, close, and rename failures into every mutator, compare pre/post in-memory state, and restart from disk. The API's reported result must match both views.

When improving the persistence helper to sync directories, distinguish failures before publication from a failed durability check after rename. The latter is an uncertain outcome: reload/reconcile or stop serving mutations rather than asserting that the old snapshot is still authoritative. This is important when combining this change with A18.

<a id="a11"></a>

### A11 — High — Public login work and audit writes have no rate, concurrency, or field-size budget

**Evidence:** Every login attempt computes 120,000 PBKDF2 rounds. `API.login` then logs the caller-supplied name on both outcomes. `Auth.Append` rewrites up to 2,000 entries under the same mutex used for authentication. Request bodies are capped at 8 MiB, but names/passwords have no practical upper bound, no login throttler is wired, and successful logins can accumulate sessions until expiry. [AUTH] [APIEXT] [APP]

**Impact:** Unauthenticated requests can consume CPU and cause repeated large audit-file writes while blocking authenticated requests on `a.mu`. Limiting the number of audit records does not limit bytes. This is separate from the already-fixed MCP password-KDF amplifier.

**Fix:** Bound authentication field lengths, use a bounded per-origin/per-account rate limiter and global hash semaphore, cap sessions per account, and give audit storage its own lock and byte budget. Do not trust forwarded client-IP headers unless the immediate proxy is explicitly trusted. Do not write an unlimited failure log for requests rejected by the limiter.

```go
var loginWork = make(chan struct{}, 4) // Tune to the smallest supported host.

func authInputOK(name, password string) bool {
    return len(name) >= 2 && len(name) <= 128 &&
        len(password) >= 8 && len(password) <= 1024
}

// Before calling Auth.Login:
if !authInputOK(body.Name, body.Password) {
    writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid credential lengths"))
    return
}
select {
case loginWork <- struct{}{}:
    defer func() { <-loginWork }()
default:
    w.Header().Set("Retry-After", "2")
    writeErr(w, http.StatusTooManyRequests, fmt.Errorf("too many authentication attempts"))
    return
}
```

This semaphore limits simultaneous work, not guessing frequency; add a bounded expiring rate-limiter table as well. For audit entries, truncate/normalize the untrusted attempted username before recording it and record it as a *claimed identifier*, not an authenticated actor. Move PBKDF2 out of `createLocked` too; that path still holds the global lock while hashing.

**Regression:** Flood unknown-user and wrong-password attempts and assert bounded CPU work, bounded audit bytes, bounded limiter memory, and continued responsiveness of an already-authenticated request.

<a id="a12"></a>

### A12 — High — Revoked sessions retain read access through existing event and console streams

**Evidence:** SSE authorization is checked only on connection. The WebSocket reader rechecks a session for each incoming command, but the writer continues sending console output without reauthorization. A revoked, expired, or deleted user's idle connection therefore continues receiving updates. [API] [AUTH]

**Fix:** Recheck the live session, minimum read role, and password-change lockout periodically and immediately before sending protected payloads. Close the stream when access is lost. Use a shared validity helper; do not trust the session pointer captured at upgrade.

```go
func (a *API) streamAuthorized(r *http.Request) bool {
    bypass, setup := a.mgr.auth.accessMode()
    if bypass { return true }
    if setup { return false }
    cookie, err := r.Cookie("gss_session")
    if err != nil { return false }
    sess := a.mgr.auth.Session(cookie.Value)
    return sess != nil && roleRank[sess.Role] >= roleRank[RoleViewer] &&
        !a.mgr.auth.MustChangePassword(sess.User)
}

// In SSE's ping branch and before each event payload:
if !a.streamAuthorized(r) { return }

// In the WebSocket writer's ticker branch and before each payload:
if !a.streamAuthorized(r) {
    _ = c.Close(websocket.StatusPolicyViolation, "session no longer authorized")
    return
}
```

For near-immediate revocation, additionally track active streams by session and cancel them from logout/password-reset/account-delete. Keep the periodic check as a backstop for expiry.

**Regression:** Open SSE and a read-only WebSocket; revoke the cookie without sending another command. Both connections must stop receiving protected information within a documented bounded interval.

<a id="a13"></a>

### A13 — Medium — Session cookies are not Secure and browser mutation requests have no explicit origin protection

**Evidence:** `setSessionCookie` sets `HttpOnly` and `SameSite=Lax` but never `Secure`. The HTTP stack has no cross-origin mutation guard, and JSON handlers accept bodies without validating their media type. The WebSocket has an origin check; that does not protect ordinary HTTP mutations or login. [APIEXT] [APP] [API]

**Impact and qualification:** SameSite=Lax is useful protection against many cross-site requests, so this is not a claim that every cross-site POST carries the cookie. It does not replace an origin policy for same-site sibling origins or login CSRF. Without Secure, a session cookie can be sent over HTTP to the same host where HTTP remains reachable.

**Fix:** Configure the public HTTPS scheme explicitly, set Secure accordingly, and use Go's cross-origin protection on browser-facing routes. Do not infer HTTPS from an arbitrary incoming `X-Forwarded-Proto` header. Keep bearer authentication on MCP independent of cookies. [HTTPDOC]

```go
// Add SecureCookies bool to the deployment configuration and API.
func setSessionCookie(w http.ResponseWriter, s *Session, secure bool) {
    http.SetCookie(w, &http.Cookie{
        Name: "gss_session", Value: s.Token, Path: "/",
        HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
        Expires: s.Expires,
    })
}

// In Run, around the browser-facing handler. Add only explicitly configured
// trusted origins; do not add a wildcard to accommodate a reverse proxy.
protection := http.NewCrossOriginProtection()
handler := protection.Handler(limitBodies(mgr.auth.attach(mux)))
```

Update both cookie-creation call sites and logout's cookie attributes. Require `application/json` on JSON mutation routes, reject trailing JSON values, and document the supported reverse-proxy arrangement. Add frame-embedding protection and `X-Content-Type-Options: nosniff` as defense in depth. Introduce a tested Content Security Policy rather than a policy that silently breaks the frontend's existing inline handlers/styles.

**Regression:** Inspect HTTPS login/logout cookie flags; reject an unsafe same-site but cross-origin request; reject a cross-origin text/plain login; retain successful same-origin requests and legitimate bearer clients without browser origin headers.

<a id="a14"></a>

### A14 — High — Streaming deadline removal reopens slow-body and stalled-writer resource exhaustion

**Evidence:** `createBackup` clears read and write deadlines before reading its JSON body and discards the decoding error. SSE and downloads remove deadlines entirely. SSE ignores write/flush failures. The WebSocket writer uses an unbounded context for payload writes; its ping ticker cannot run while that same goroutine is stuck writing. A per-stream subscriber cap exists, but does not bound how long a stalled client occupies a slot. [APIEXT] [API] [APP]

**Fix:** Parse and validate the request while its read deadline is still armed. Permit an empty optional body only on EOF. Afterward remove only the absolute response deadline and replace it with a rolling per-write deadline. Bound WebSocket writes independently of ping timeouts.

```go
// createBackup: before changing connection deadlines.
var body struct{ Note string }
if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
    writeErr(w, http.StatusBadRequest, err)
    return
}
if len(body.Note) > 4096 {
    writeErr(w, http.StatusBadRequest, fmt.Errorf("backup note is too long"))
    return
}

func sendSSE(w http.ResponseWriter, payload []byte) error {
    rc := http.NewResponseController(w)
    if err := rc.SetWriteDeadline(time.Now().Add(10*time.Second)); err != nil {
        return err
    }
    if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
        return err
    }
    return rc.Flush()
}

// Use on every WebSocket write, including acknowledgements and drop notices.
func writeConsole(ctx context.Context, c *websocket.Conn, payload []byte) error {
    writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    return c.Write(writeCtx, websocket.MessageText, payload)
}
```

Apply equivalent rolling deadlines to download chunks and SSE comments. Handle errors by terminating the stream. For a long-running backup job, prefer an asynchronous operation resource instead of holding an idle HTTP response open. See A38 for the same timeout/result mismatch in plugin installation.

**Regression:** Send complete backup headers followed by a stalled body; the request must time out without creating a backup. Open a client that stops reading SSE/WebSocket/download output and verify prompt resource release. Malformed or oversized optional backup JSON must not be accepted.

<a id="a15"></a>

### A15 — High — Shared filesystem locks do not serialize configuration transactions against one another

**Evidence:** `WriteFile`, resource changes, and property-file writes use shared `fsMu.RLock` sections. That excludes backups/restores, but permits multiple writers. `WriteFile` snapshots old bytes, publishes new bytes, then changes the port and reloads the model. Two such operations can interleave publication and model commits, or a failed operation's rollback can overwrite a newer successful edit. [FILES] [MANAGER]

**Fix:** Add a separate per-server mutation mutex, or use an exclusive transaction gate for all cooperating configuration mutations. Keep one documented lock order and split already-locked helpers; recursively acquiring a mutex/RWMutex is not the solution.

```go
// Server: new field. Protects cooperating file/settings/plugin transactions.
editMu sync.Mutex

// Public configuration mutation entry point:
s.fsMu.RLock() // Excludes lifecycle launches and snapshot/restore windows.
defer s.fsMu.RUnlock()
s.editMu.Lock() // Serializes edits with each other.
defer s.editMu.Unlock()
if err := m.requireRegistered(s); err != nil { return err }
// Read old bytes/model, validate the complete candidate, publish and persist.
// Call held helpers here, never methods that reacquire editMu or fsMu.
```

Apply the same order to `ApplySettings`, `WriteFile`, plugin publication/toggling, and relevant player-list/property writes. A restore that already holds `fsMu` exclusively can use the held helpers. External game/plugin writes are not serialized by this mutex; either prohibit conflicting edits while the game runs or detect stale revisions explicitly.

**Regression:** Synchronize two writes to different port values with barriers between file publication and model adoption. After both finish, disk, `s.Port`, `s.Props`, and persisted state must agree. A failed edit must not roll back a different completed edit.

<a id="a16"></a>

### A16 — High — Properties-file identity and parsing disagree across validation, publication, and reload

**Evidence:** `WriteFile` treats any path whose **basename** is `server.properties` as the server's root configuration. Editing `plugins/example/server.properties` can therefore change the panel's global server model. `validatePropsPort` returns on the first `server-port` occurrence, whereas `reloadProps` builds a map in which the last occurrence wins. Reload also updates only already-known keys and does not remove deleted keys, so subsequent settings writes can restore removed keys or discard newly added ones. [FILES]

**Fix:** Special handling must require the exact normalized root-relative path. Use one parser and one candidate snapshot for validation, storage, and model adoption. A full Java-properties parser must support its actual escape/continuation rules; a smaller supported format should reject ambiguous input rather than interpret it differently in different paths.

```go
// In WriteFile, replace every basename comparison:
isRootProperties := name == "server.properties"

// Minimal strict format for the panel-managed editor. This deliberately
// rejects duplicate keys and unsupported syntax instead of guessing.
func parsePanelProperties(content string) (map[string]string, error) {
    out := make(map[string]string)
    for index, line := range strings.Split(content, "\n") {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
            continue
        }
        if strings.Contains(line, `\`) {
            return nil, fmt.Errorf("line %d: escaped properties require the full parser", index+1)
        }
        key, value, ok := strings.Cut(line, "=")
        key = strings.TrimSpace(key)
        if !ok || key == "" || strings.ContainsAny(key, " :\t") {
            return nil, fmt.Errorf("line %d: expected an unambiguous key=value", index+1)
        }
        if _, exists := out[key]; exists {
            return nil, fmt.Errorf("duplicate property %q", key)
        }
        out[key] = strings.TrimSpace(value)
    }
    return out, nil
}
```

The strict parser is a safe restricted-mode implementation, **not** a transparent replacement for every existing Minecraft configuration. For unrestricted imports, implement/use a tested Java-properties parser. Preserve unknown keys, define how missing managed keys map to game defaults, and validate all port-affecting values before publishing any bytes. Do not silently swallow a refused port adoption and return success with different bytes on disk.

**Regression:** Nested properties files, duplicate ports, escaped separators, continuations, missing keys, and newly added keys must produce consistent model/disk results or a clear refusal before mutation.

<a id="a17"></a>

### A17 — Medium — File-read and directory-list limits are enforced after unbounded work

**Evidence:** `ReadFile` checks descriptor size and then calls unbounded `io.ReadAll`; a concurrently growing file can exceed the advertised limit. It rejects directories but not other nonregular files. `ListFiles` calls `ReadDir(-1)` before limiting output to 2,000 entries, and truncates before sorting without exposing pagination/overflow. Plugin listing likewise reads the whole directory. [FILES] [PLUGINS]

**Fix implementation:** Use A08's nonblocking regular-file opener and bound bytes actually consumed. Enumerate directory entries in bounded batches; return an explicit continuation/overflow indication rather than silently hiding entries.

```go
f, _, err := openRegularIn(root, name)
if err != nil { return "", err }
defer f.Close()
b, err := io.ReadAll(io.LimitReader(f, maxEditBytes+1))
if err != nil { return "", err }
if len(b) > maxEditBytes {
    return "", fmt.Errorf("file exceeds the %d-byte editor limit", maxEditBytes)
}
return string(b), nil

// Bounded one-page directory result; update the API contract to expose more.
entries, err := directory.ReadDir(maxListItems + 1)
if err != nil && err != io.EOF { return nil, err }
more := len(entries) > maxListItems
if more { entries = entries[:maxListItems] }
// Return {entries: ..., truncated: more}; add stable cursor pagination before
// claiming the user can browse the whole directory in sorted pages.
```

Also reject special files in downloads. For stable sorted pagination on very large directories, use a bounded index or explicitly document snapshot/cursor behavior; simply sorting each raw batch does not create a globally sorted directory listing.

**Regression:** Grow a file during reading, create a FIFO, and browse a directory above the limit. Assert bounded memory/time and an explicit indication that more entries exist.

<a id="a18"></a>

### A18 — Medium — Atomic replacement does not preserve all file metadata or complete directory durability

**Evidence:** `writeAtomicIn` publishes a new inode with the caller's fixed mode (typically 0644), preserves ownership only best-effort, and does not sync the containing directory after rename. Extraction uses fixed 0644/0755 modes regardless of archived executable bits. A chmod-restricted file or executable script can change behavior after an edit/restore. The archive publication path also links/unlinks without syncing its parent directory. [FILES] [BACKUP]

**Fix:** Preserve the intended permission bits for replacement files, fail on required ownership-transfer errors, and sync directory entries after publication. Restore safe permission bits from the archive while deliberately stripping setuid/setgid/sticky bits unless explicitly needed.

```go
// During a replacement, before publishing the temporary file:
mode := os.FileMode(0o644)
if old, err := root.Stat(name); err == nil {
    mode = old.Mode().Perm()
} else if !errors.Is(err, os.ErrNotExist) {
    return err
}
// Change preserveOwnerIn to return an error; do not discard root.Chown failure.
if err := preserveOwnerInChecked(root, tmp, name); err != nil { return err }
if err := tempFile.Chmod(mode); err != nil { return err }
if err := tempFile.Sync(); err != nil { return err }
if err := tempFile.Close(); err != nil { return err }
if err := root.Rename(tmp, name); err != nil { return err }
parent, err := root.Open(path.Dir(name))
if err != nil { return err }
return errors.Join(parent.Sync(), parent.Close())
```

`preserveOwnerInChecked` should retain the existing owner-selection logic but return owner lookup/chown errors where ownership is required. Perform owner changes before setting final permission bits, then sync. A directory-sync failure **after** rename is an uncertain durability outcome, not proof that the old file remains unchanged; expose that distinction to callers instead of blindly rolling back concurrent state.

**Regression:** Edit a 0600 file and an executable script, restore executable permissions, inject chown and directory-sync failures, and verify neither silent permission broadening nor false “unchanged” claims.

<a id="a19"></a>

### A19 — High — Port reservations leak after creation and unrelated operations share a reservation identity

**Evidence:** `claimPortBindings` records every candidate port in `reservedPorts` and appends them under `reservedByWho[who]`. `Create` supplies the server's display name as `who`, but after registration deletes only `reservedPorts[basePort]` and disables deferred release. Extra/span ports and the owner record remain. `releasePort(base)` releases all ports associated with the same display name, which need not identify one operation. [MANAGER]

**Impact:** Creating and then deleting a span/extra-port server can leave ports unavailable until panel restart. Concurrent operations with the same display name can release each other's reservations. Failed provisional saves can leave the same leaked extra reservations.

**Fix implementation:** Replace display-name ownership with a unique operation lease and release the complete lease atomically with registration. Implementation R2 supplies the helper and call-site changes.

```go
leaseID, err := randomHex(16)
if err != nil { return nil, err }
if holder, ok := m.claimPortBindings(candidate, leaseID); !ok {
    return nil, fmt.Errorf("port reservation conflicts with %s", holder)
}
defer m.releaseReservation(leaseID)

// While m.mu is held at provisional registration:
m.servers[s.ID] = s
m.order = append(m.order, s.ID)
m.releaseReservationLocked(leaseID) // All span/extra members, not just base.
```

Apply the lease protocol consistently to create, clone, and import; inspect their success, failure, and cancellation exits rather than retaining any base-port-only release call. Human-readable owner labels may be separate metadata, never the lease identity.

**Regression:** Create/delete a multi-port server and immediately reuse all its ports; fail the save after registration; and overlap two same-name operations, releasing one while the other must remain reserved.

<a id="a20"></a>

### A20 — High — Several live Server reads bypass the mutex that protects their writes

**Evidence:** `claimPortBindings` calls `serverBindingsLocked(s)` while holding only the manager mutex, although that helper reads fields protected by `s.mu`. `validatePropsPort` does the same for other servers. `serverMetrics` reads `s.MemoryMB`/`s.CPU` directly, and `patchServer` reads them for the audit message after `SetResources` releases the server lock. Resource and live-binding writers use `s.mu`, not just `m.mu`. [MANAGER] [FILES] [APIEXT] [API]

**Impact:** Concurrent HTTP/resource/lifecycle activity can produce data races and inconsistent port-admission decisions. Passing existing race tests does not establish that these particular interleavings were exercised.

**Fix implementation:** The manager mutex protects membership, not the contents of a server. Use the locking wrapper at call sites that do not already own `s.mu`, and snapshot scalar response fields under that same server mutex.

```go
// claimPortBindings / validatePropsPort, while manager membership is stable:
if bindingConflict(serverBindings(s), candidate) {
    // ... refuse conflict ...
}

// serverMetrics:
s.mu.Lock()
limitMB, limitCPU := s.MemoryMB, s.CPU
s.mu.Unlock()
// Use limitMB and limitCPU in the existing response object instead of
// accessing s.MemoryMB and s.CPU after releasing the lock. Retain the existing
// metrics-query method and response field names unchanged.
```

Use an analogous snapshot for the resource-change audit entry. Document `manager.mu -> server.mu` where both are needed and verify no inverse acquisition path is introduced. Do not replace a correctly already-locked call with a wrapper that recursively locks the same mutex.

**Regression:** Race admission against binding changes and race metrics/audit responses against repeated resource updates under `go test -race`. Tests must call the real HTTP handlers and admission methods.

<a id="a21"></a>

### A21 — High — Server deletion ignores persistence and filesystem failures after removing worlds and backups

**Evidence:** `Delete` removes the server from the manager, drops tasks/room/metrics, deletes both the managed server directory and backup directory with ignored errors, then ignores `m.Save()` and returns success. [MANAGER]

**Impact:** Deletion may be reported successful while data or metadata remains. Conversely, files can be gone while old `servers.json` still references them, allowing a restart to resurrect an entry with missing data. There is no durable deletion record to distinguish interrupted cleanup from a valid server.

**Fix:** Use a durable deletion transaction/tombstone with an explicit scope (detach versus delete managed data/backups). Record intent before irreversible cleanup, exclude tombstoned entries from startup/adoption, perform checked cleanup, and retain failed tombstones for retry. Never resolve an adopted source symlink and delete its target as a side effect of deleting the panel's link.

```go
// Integration shape: implement deletionStore in the private state directory.
intent := DeleteIntent{
    ServerID: s.ID, RemoveManagedData: true, RemovePanelBackups: true,
}
if err := deletionStore.Commit(intent); err != nil { return err }
// With fsMu/lifecycle held, durably publish a server list excluding s.
// This method must write the prospective list before publishing membership.
if err := m.commitServerRemoval(s.ID); err != nil { return err }
if err := os.RemoveAll(filepath.Join(m.dataDir, "servers", s.ID)); err != nil {
    return fmt.Errorf("server detached; data cleanup pending: %w", err)
}
if err := os.RemoveAll(m.backupDir(s.ID)); err != nil {
    return fmt.Errorf("server detached; backup cleanup pending: %w", err)
}
return deletionStore.Complete(s.ID)
```

`DeleteIntent`, `deletionStore`, and `commitServerRemoval` are new integration components: the record must be atomic/durable, startup must honor it, and task cleanup must be recorded/retried too. Alternatively make the initial release's API detach-only and expose permanent deletion as a separate operation. Do not claim a transaction from simply adding an error check after destruction.

**Regression:** Crash or fail after each deletion step. Startup must neither resume a tombstoned server nor destroy an adopted external tree; the UI must distinguish complete deletion from pending cleanup.

<a id="a22"></a>

### A22 — Medium — Queued operations can recreate a server directory after the server was deleted

**Evidence:** HTTP handlers obtain a `*Server` before calling file/plugin methods. Mutations can wait on `fsMu` while `Delete` removes that server. When the waiter continues, `serverRoot` calls `ensureServerDir`, which recreates the directory; there is no post-lock membership/identity validation. Reads also use this create-on-open path. Plugin installation opens the root before acquiring its filesystem gate. [FILES] [PLUGINS] [MANAGER]

**Fix implementation:** Validate object identity after acquiring the server gate and before any creation/open. Read-only paths should not create missing roots. Move plugin root acquisition inside the protected, validated section.

```go
func (m *Manager) requireRegistered(s *Server) error {
    m.mu.RLock()
    same := m.servers[s.ID] == s
    m.mu.RUnlock()
    if !same { return fmt.Errorf("server no longer exists") }
    return nil
}

// After acquiring fsMu, before ensureServerDir/serverRoot:
if err := m.requireRegistered(s); err != nil { return err }

// Read-only root opening: a missing directory is an error, not a mkdir.
func (m *Manager) existingServerRoot(s *Server) (*os.Root, error) {
    if err := m.requireRegistered(s); err != nil { return nil, err }
    return os.OpenRoot(m.serverDir(s))
}
```

Membership validation and opening must occur under a gate that excludes `Delete`; a standalone check without that gate remains check-then-act. Keep separate creation helpers for a new server that is intentionally not registered yet.

**Regression:** Pause a write or plugin install before gate acquisition, delete its server, and release the waiter. It must fail without recreating a directory or publishing a ghost entry.

<a id="a23"></a>

### A23 — High — A failed Docker kill cancels supervision of a container that may still be running

**Evidence:** `dockerRunner.Kill` calls `cancelProc(s)` unconditionally, even when `docker kill` fails. The manager may then set status back to `running` after an inspection, but the log/stats/exit watchers have already been cancelled. The stop path correctly cancels only after success; the kill path does not share that guarantee. [RUNNER] [MANAGER]

**Fix implementation:**

```go
func (r *dockerRunner) Kill(s *Server) error {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    name := containerPrefix + "-" + s.ID
    out, err := exec.CommandContext(ctx, "docker", "kill", name).CombinedOutput()
    if err != nil {
        return fmt.Errorf("could not kill container: %w: %s", err, strings.TrimSpace(string(out)))
    }
    r.cancelProc(s)
    return nil
}
```

Use the bounded-output executor in A25 in production, and derive the context from application/operation ownership rather than a permanent background context. On failure, retain supervision and determine state using an error-aware inspection, not a false-on-error boolean.

**Regression:** Make the kill command fail while inspection reports the container alive. Existing supervision must remain attached and a later real exit must still be observed.

<a id="a24"></a>

### A24 — High — Docker transport errors are confused with process death, and exit 137 is always labeled OOM

**Evidence:** `containerRunning` returns false on any inspection error. `watchExit` reports a non-cancellation `docker wait` command failure as a game-process exit; the manager can clear its process/bindings even though the container survived a daemon/CLI communication failure. Startup uses the same false-on-error inspection before a force-remove of a presumed dead container. The exit watcher also labels every code 137 as OOM. [RUNNER] [MANAGER]

**Fix:** Represent `running`, `stopped`, and `unknown/error` separately. A watcher transport failure should trigger supervision reconnection, not a fabricated game exit. Never force-remove a container unless a successful inspection has established an allowed terminal state. Distinguish a known missing container from an unreachable daemon using structured Docker API errors or an explicit checked lookup.

```go
// A minimal CLI implementation preserves uncertainty. Missing-container errors
// remain errors here; callers must not reinterpret them as "safe to remove".
type inspectedState struct {
    Running   bool `json:"Running"`
    OOMKilled bool `json:"OOMKilled"`
    ExitCode  int  `json:"ExitCode"`
}
func inspectState(ctx context.Context, name string) (inspectedState, error) {
    var state inspectedState
    out, err := exec.CommandContext(ctx, "docker", "inspect", "--format",
        "{{json .State}}", name).Output()
    if err != nil { return state, fmt.Errorf("inspect container: %w", err) }
    if err := json.Unmarshal(out, &state); err != nil { return state, err }
    return state, nil
}

// After a wait/transport error:
state, err := inspectState(boundedContext, name)
if err != nil {
    // Retain bindings/handle; mark supervision unknown and retry with backoff.
    return err
}
if state.Running { return errReconnectSupervision }
reason := "exited"
if state.OOMKilled { reason = "OOM" }
// Only now report the confirmed exit; record intentional kill/stop separately.
```

The retry loop and `errReconnectSupervision` are new supervision integration points. Avoid letting its return fall through the existing unconditional watcher-context cancellation path. Adoption must re-establish all watchers, not merely set the displayed status.

**Regression:** Interrupt the Docker connection without stopping the game, fail inspection before startup, and compare a deliberate SIGKILL with a container whose inspected `OOMKilled` is true. Unknown state must not release ports or permit destructive cleanup.

<a id="a25"></a>

### A25 — Medium — Docker control calls and captured output are not consistently bounded or cancellable

**Evidence:** Docker info/images/inspect/stop/exec/stats calls frequently use `exec.Command` without context deadlines. Console commands hold the process mutex across an unbounded `CombinedOutput`. A stuck daemon can stall an operation indefinitely and retain filesystem/lifecycle-related resources; excessive command output can consume unbounded memory. [RUNNER] [MANAGER]

**Fix implementation:** Centralize bounded control-command execution. Keep `docker wait` and log following as intentionally long-lived, application-cancellable streams, not ordinary short commands.

```go
type cappedOutput struct {
    buf bytes.Buffer
    max int
    truncated bool
}
func (w *cappedOutput) Write(p []byte) (int, error) {
    original := len(p)
    room := w.max - w.buf.Len()
    if room < len(p) { w.truncated = true; p = p[:max(0, room)] }
    _, _ = w.buf.Write(p)
    return original, nil // Continue draining so the child cannot block on its pipe.
}
func dockerControl(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
    ctx, cancel := context.WithTimeout(parent, timeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, "docker", args...)
    cmd.WaitDelay = 2*time.Second
    out := &cappedOutput{max: 1 << 20}
    cmd.Stdout, cmd.Stderr = out, out
    err := cmd.Run()
    if ctx.Err() != nil { return out.buf.Bytes(), ctx.Err() }
    if out.truncated {
        err = errors.Join(err, fmt.Errorf("Docker output exceeded 1 MiB"))
    }
    return out.buf.Bytes(), err
}
```

Because stdout and stderr reference the same writer, `os/exec` serializes writes to that writer; [EXECDOC] do not reuse it across unrelated commands. Choose operation-specific deadlines (for example, stop needs more than its 45-second grace period). A timeout after a mutation is an **unknown result** until reconciled, not proof that the container operation never happened.

**Regression:** Use a fake Docker executable that stalls or emits excessive output. Assert cancellation, bounded memory, released locks, and reconciliation of a timed-out mutation.

<a id="a26"></a>

### A26 — Medium — Oversized or interrupted Docker log output silently ends the console reader

**Evidence:** `dockerRunner.stream` caps `bufio.Scanner` at 1 MiB but never checks `sc.Err()`. Its caller then waits for the `docker logs -f` process. A too-long line can stop pipe consumption while the child is still producing output, leaving the wait stuck; ordinary stream failure also has no reconnect behavior. [RUNNER]

**Fix:** Consume oversized lines in bounded fragments, discard the remainder of just that line, and emit an explicit truncation warning. Distinguish log-transport failure from game exit and reconnect log following with a bounded backoff/cursor policy.

```go
func readLogLine(r *bufio.Reader, limit int) (line []byte, truncated bool, err error) {
    for {
        fragment, more, readErr := r.ReadLine()
        room := limit-len(line)
        if room > 0 {
            n := min(room, len(fragment))
            line = append(line, fragment[:n]...)
        }
        if len(fragment) > max(0, room) { truncated = true }
        if readErr != nil { return line, truncated, readErr }
        if !more { return line, truncated, nil }
    }
}

// stream uses a 64-KiB bufio.Reader and this helper instead of Scanner.
// A truncated line still gets drained through its newline, then reading resumes.
```

On terminal reader error, log the reason, close/cancel the log child, and join it. Restart only the log follower when appropriate; do not route that error through the container-exit handler. Add bounded timestamps/sequence handling to prevent replay duplicates on reconnect.

**Regression:** Emit a line longer than 1 MiB followed by a normal line. The normal line must remain visible and the log child must be reapable. Simulate a daemon log-stream disconnect while the game remains alive.

<a id="a27"></a>

### A27 — High — MCP console restrictions are a bypassable first-word denylist, not a capability boundary

**Evidence:** `SendCommand` rejects only exact first words such as `op`, `ban`, and `stop`. `consoleVerb` does not understand namespaces, aliases, wrappers, or plugin-provided command semantics. Namespaced commands are supported in the Paper/Bukkit ecosystem. A different first word can therefore reach the same restricted operation on compatible servers. Newline rejection is useful but does not solve semantic dispatch. [MCP] [PAPERCOMMANDS]

**Impact and qualification:** This undermines the documented restriction that access-changing commands remain a human panel decision. The exact bypass depends on the installed server/command set; no live server command was executed. It is not an unauthenticated endpoint: a valid MCP token is required, subject to A01's separate issuance defect.

**Fix:** Disable unrestricted raw console execution by default and expose typed, scoped actions. Maintain a small positive allowlist only for a verified server adapter; extending the denylist with another spelling is not a reliable fix.

```go
// Secure default: existing raw-console MCP tool no longer forwards arbitrary text.
func (b mcpBackend) SendCommand(id, text string) (string, error) {
    return "", fmt.Errorf("raw console commands are disabled over MCP; use typed tools")
}

// Example typed query, exposed with a server-scoped read capability:
func (b mcpBackend) ListOnlinePlayers(id string) (string, error) {
    s, err := b.server(id)
    if err != nil { return "", err }
    switch s.Template {
    case "paper", "purpur", "spigot", "vanilla":
        // These adapters implement the Java-edition player-list query.
    default:
        return "", fmt.Errorf("unsupported server adapter")
    }
    dr, ok := b.m.runnerFor(s).(*dockerRunner)
    if !ok { return "", fmt.Errorf("a real server connection is required") }
    return dr.query(s, "list") // No caller-controlled command syntax.
}
```

Update the MCP tool schema/dispatch to expose only implemented typed tools; the transport/dispatcher in `internal/mcp` was not fully audited here. Carry token identity and server/action scopes into the backend and audit log instead of attributing every action simply to `mcp`. Human confirmation for exceptional actions must be enforced by a trusted application flow, not by a model prompt.

**Regression:** A valid token cannot execute privileged behavior through namespaced verbs, aliases, wrappers, or plugin commands. Test typed tools independently of the language model using them.

<a id="a28"></a>

### A28 — Medium — Duplicate MCP token names make revocation ambiguous and incomplete

**Evidence:** `Issue` permits duplicate names; `Revoke(name)` removes only the first matching record. Listing and HTTP revocation identify tokens by that name, so another credential with the displayed same name can remain usable after revocation. [MCP]

**Fix:** Give each token a stable random ID and revoke by ID. As a compatible immediate fix, reject duplicate normalized names and revoke every legacy duplicate in one persist-before-publish operation.

```go
func (t *mcpTokens) Revoke(name string) error {
    t.mu.Lock()
    defer t.mu.Unlock()
    normalized := strings.TrimSpace(name)
    next := make([]mcpToken, 0, len(t.toks))
    for _, token := range t.toks {
        if strings.TrimSpace(token.Name) != normalized { next = append(next, token) }
    }
    if len(next) == len(t.toks) { return fmt.Errorf("no such token") }
    data, err := json.MarshalIndent(next, "", "  ")
    if err != nil { return err }
    if err := writeFileAtomic(t.path, data, 0o600); err != nil { return err }
    t.toks = next
    return nil
}
```

In `Issue`, under the same lock, reject a matching normalized name before writing a prospective slice; enforce a name-length limit and return the plaintext only after persistence succeeds. Token expiry and per-server/action scopes are additional hardening, not already-implemented controls.

**Regression:** Seed two legacy records with the same name, revoke it, and verify both secrets fail authentication. Concurrent same-name issuance must either create one token or require distinct IDs unambiguously.

<a id="a29"></a>

### A29 — Medium — Task creation uses collision-prone IDs, borrows mutable input, and accepts server-owned metadata

**Evidence:** Scheduler IDs concatenate a Unix-time value modulo 100,000 with a sequence modulo 100. Repeated time/sequence combinations can collide. `Add` stores and returns the supplied pointer, overrides Enabled to true, and leaves caller-provided run metadata intact. The HTTP handler decodes directly into `Task`. A returned pointer can be serialized while the scheduler changes its bookkeeping. [SCHED] [APIEXT]

**Fix:** Decode a create-only request, construct a new task, generate an unpredictable unique ID, preserve the requested enabled state, reset execution metadata, persist a copied candidate list, and return a copy.

```go
type createTaskRequest struct {
    Name string `json:"name"`
    Commands string `json:"commands"`
    Time string `json:"time"`
    Repeat bool `json:"repeat"`
    Enabled *bool `json:"enabled"`
}

// Inside Add's replacement, after request validation and under sc.mu:
suffix, err := randomHex(16)
if err != nil { return nil, err }
enabled := request.Enabled == nil || *request.Enabled
created := &Task{
    ID: "t_"+suffix, ServerID: serverID, Name: request.Name,
    Commands: request.Commands, Time: request.Time,
    Repeat: request.Repeat, Enabled: enabled,
}
// Check uniqueness; persist a prospective copied slice as described in A10.
// Do not store the caller's pointer or accept Runs/LastRun/LastErr from it.
result := *created
return &result, nil
```

The final return belongs after the new candidate slice is durably committed and published. Keep task creation coordinated with server deletion so a task cannot become orphaned between its server-existence check and insertion.

**Regression:** Generate more than 100 tasks in the same second, simulate restart/time reuse, submit forged run metadata, create a disabled task, and serialize a creation result concurrently with task execution.

<a id="a30"></a>

### A30 — Medium — Task mutation routes ignore the server ID in their URL and can misattribute audit records

**Evidence:** Update/delete/run routes take `tid` into global scheduler methods without verifying that the task belongs to URL parameter `id`. The update/delete audit target is derived from that unverified URL parameter. [APIEXT] [SCHED]

**Impact:** A stale or mistaken URL can modify a task on a different server and attribute the action to the wrong server. The current roles are fleet-wide, so this is a resource-identity/audit defect, not evidence of a current per-tenant authorization bypass.

**Fix implementation:** Enforce both identifiers in scheduler mutation methods under the scheduler lock, not only in a separate handler precheck.

```go
// Add serverID to Update/Delete/Run's scoped API and check within the same
// critical section used to locate/mutate the task.
func findTaskIndex(tasks []*Task, serverID, taskID string) int {
    for i, task := range tasks {
        if task.ID == taskID && task.ServerID == serverID { return i }
    }
    return -1
}

// Handler integration:
updated, err := a.mgr.sched.UpdateForServer(r.PathValue("id"), r.PathValue("tid"), mutate)
if err != nil { writeErr(w, http.StatusNotFound, err); return }
a.mgr.audit(actorOf(r), "task.update", updated.ServerID, updated.Name)
```

Implement `UpdateForServer` using the existing Update body with the two-field match and A10's prospective persistence. For Run, snapshot the matching task under lock before scheduling it; audit the matched task's server, not the request string.

**Regression:** Every mismatched `(serverID, taskID)` pair must return not found, leave the task unchanged, and never create an audit entry naming the wrong server.

<a id="a31"></a>

### A31 — Medium — Scheduler civil-time calculations disagree with actual firing at DST and day boundaries

**Evidence:** `NextRun` adds elapsed seconds to midnight and advances by 24 hours. The loop instead compares local hour/minute/second in a 60-second window and suppresses repeats only for two elapsed minutes. These are not equivalent around daylight-saving changes, midnight, delayed ticks, or repeated local hours. An isolated Vancouver probe showed 03:30 rendered as 04:30 on March 8, 2026. [SCHED]

**Fix:** Compute civil occurrences with `time.Date` and `AddDate`, persist an occurrence identity, and make preview and execution use the same function. Decide/document missed-occurrence and repeated/nonexistent-time policies. A simple safe policy is once per local date, skip nonexistent times, and run missed occurrences only within a configured grace window.

```go
func nextCivilOccurrence(clock string, now time.Time) (time.Time, error) {
    seconds, err := parseClock(clock)
    if err != nil { return time.Time{}, err }
    h, m, s := seconds/3600, (seconds%3600)/60, seconds%60
    for day := 0; day < 370; day++ {
        date := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0,
            now.Location()).AddDate(0, 0, day)
        candidate := time.Date(date.Year(), date.Month(), date.Day(), h, m, s, 0,
            now.Location())
        // Go normalizes a nonexistent wall time; skip it explicitly.
        if candidate.Hour() != h || candidate.Minute() != m || candidate.Second() != s {
            continue
        }
        if candidate.After(now) { return candidate, nil }
    }
    return time.Time{}, fmt.Errorf("no civil occurrence found")
}

// Persist before starting a scheduled occurrence, under the task writer lock:
occurrenceID := task.ID + ":" + due.In(location).Format("2006-01-02")
// Do not execute the same occurrenceID twice on a repeated local hour.
```

Persist `claimed/running/completed/unknown` execution state; a crash after sending a command cannot provide general exactly-once semantics. Do not automatically replay an uncertain destructive action. Make one-shot scheduling explicit (including a date) rather than previewing “no next run” for a task the next day's clock loop will execute.

**Regression:** Spring-forward and fall-back days, 23:59:59, a delayed tick, restart within/after the grace window, and one-shot times already passed. Displayed next-run values must match actual admission decisions.

<a id="a32"></a>

### A32 — Medium — Scheduled executions have no application cancellation or global work budget

**Evidence:** Each due task starts a goroutine. The running map prevents duplicate execution of the *same* task, but not an arbitrary number of distinct tasks. `!wait` sleeps without context, and a task may contain many such steps. Deleting/disabling a task does not cancel an already-copied run, and panel shutdown has no owned cancellation path. [SCHED] [APP]

**Classification:** Operational hardening and missing lifecycle semantics, not a claim that disabling a schedule must universally cancel its current run. The application should state which behavior it implements.

**Fix implementation:** Bound concurrent executions, thread a context through steps, provide explicit cancellation, and give each run a status record. Keep a separate option for disabling future occurrences.

```go
// Scheduler fields, initialized per application instance:
slots chan struct{} // e.g. make(chan struct{}, 4)

func waitStep(ctx context.Context, seconds int) error {
    timer := time.NewTimer(time.Duration(seconds)*time.Second)
    defer timer.Stop()
    select {
    case <-ctx.Done(): return ctx.Err()
    case <-timer.C: return nil
    }
}

// Before admitting a run; do not create unlimited goroutines waiting for slots.
select {
case sc.slots <- struct{}{}:
    defer func() { <-sc.slots }()
default:
    return fmt.Errorf("task execution capacity is busy")
}
```

Add `Run(ctx, serverID, taskID, actor)` and `step(ctx, ...)`; carry cancellation into Docker calls and backup/import jobs. Enforce command-count and total-run-duration limits, with explicit exceptions for supported long jobs. Cancellation must not skip essential save-resume or restore-recovery cleanup.

**Regression:** Admit more distinct tasks than capacity, cancel a waiting step, disable future runs without cancelling the current one, explicitly cancel a run, and shut down the application while workers are active.

<a id="a33"></a>

### A33 — Medium — Run does not own worker shutdown, starts work before binding, and formats IPv6 addresses incorrectly

**Evidence:** `Run` launches metrics, sampling, guard, player-sync, scheduler, and session-reaper goroutines before `ListenAndServe`; state load can also start boot recovery. There is no caller context, join mechanism, or graceful shutdown. Several configuration values are package globals. The listener address is formatted as `"%s:%d"`, which does not bracket an IPv6 host. Worker ownership is also the remaining explicitly deferred item in the repository's prior audit register. [APP] [MAIN] [MANAGER] [AUDITOPEN]

**Fix:** Bind and validate before starting long-lived work, put instance configuration on the application/manager, pass an application context to every worker, and join on shutdown. Use `net.JoinHostPort`. Preserve the intentional policy that Docker game containers survive a panel restart; stop panel-owned watcher clients, not the games themselves.

```go
func servePanel(ctx context.Context, cfg Config, handler http.Handler,
    workers []func(context.Context)) error {
    addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
    listener, err := net.Listen("tcp", addr)
    if err != nil { return err }
    defer listener.Close()
    appCtx, cancel := context.WithCancel(ctx)
    defer cancel()
    var wg sync.WaitGroup
    for _, worker := range workers {
        wg.Add(1)
        go func(fn func(context.Context)) {
            defer wg.Done()
            fn(appCtx)
        }(worker)
    }
    server := newHTTPServer(addr, handler)
    server.BaseContext = func(net.Listener) context.Context { return appCtx }
    stopped := make(chan struct{})
    go func() {
        defer close(stopped)
        <-appCtx.Done()
        shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
        defer stop()
        if err := server.Shutdown(shutdownCtx); err != nil { _ = server.Close() }
    }()
    serveErr := server.Serve(listener)
    cancel()
    <-stopped
    wg.Wait()
    if errors.Is(serveErr, http.ErrServerClosed) { return nil }
    return serveErr
}
```

This is a serving core, not a complete replacement for Run: bind **before** any initialization that can spawn recovery work, or split loading from starting recovery. Convert each ticker loop to select on context. WebSocket contexts currently use Background; attach them to application cancellation and explicitly close hijacked connections, because `http.Server.Shutdown` does not own them. In main, use `signal.NotifyContext` and handle the returned error without treating normal cancellation as fatal.

**Regression:** Occupied port, IPv6 loopback, early initialization failure, cancellation with open streams, and two application instances. Verify all panel-owned workers exit while the declared game-container continuity policy is preserved.

<a id="a34"></a>

### A34 — Medium — Third-party player-head requests are enabled by default despite an opt-in privacy claim

**Evidence:** The frontend comment says player names do not leave the LAN unless the operator turns the feature on. `headsEnabled()` instead returns true whenever localStorage is anything other than `'off'`, including a fresh browser with no setting. Rendering player heads therefore sends names to the configured external avatar service by default. A local JavaScript evaluation confirmed the fresh-setting expression evaluates true. [FRONTEND]

**Fix implementation:** Make opt-in explicit, handle unavailable storage, and avoid disclosing a referrer. Update the UI copy to say the browser contacts a third party and which identifier is sent.

```javascript
function headsEnabled() {
  try { return localStorage.getItem(HEADS_KEY) === 'on'; }
  catch { return false; }
}

// When constructing the avatar image, preferably using DOM APIs:
const image = document.createElement('img');
image.src = `${HEADS_URL}/${encodeURIComponent(name)}/32`;
image.alt = '';
image.loading = 'lazy';
image.referrerPolicy = 'no-referrer';
image.addEventListener('error', () => image.remove(), { once: true });
```

Do not silently convert existing unset values to consent. A deployment-level “external requests disabled” setting should override local opt-in where appropriate.

**Regression:** A fresh profile, blocked localStorage, and a disabled deployment policy must make zero avatar-service requests. Only an explicit opt-in may cause them.

<a id="a35"></a>

### A35 — Medium — The delete confirmation understates destruction of server data and backups

**Evidence:** The frontend asks, “Delete this server? Its state is removed from the panel.” `Manager.Delete` actually removes the managed server directory **and** its panel backup directory, not just the list entry. For an adopted tree, unlinking the panel symlink is different from deleting the external source; the UI does not explain that distinction. [FRONTEND] [MANAGER]

**Fix:** Separate detach from irreversible data deletion, preserve backups by default, and describe exact scope before confirming. A minimal truthful confirmation for the existing destructive endpoint is:

```javascript
const server = serverById(id);
if (!server) { toast('This server no longer exists.', 'err'); return; }
const entered = prompt(
  `Permanently delete ${server.name}?\n\n` +
  'This removes the panel-managed server directory and ALL panel backups for it.\n' +
  'An adopted external source directory is not deleted by unlinking its panel entry.\n' +
  'There is no undo. Copy any required backups outside the panel first.\n\n' +
  `Type the server name exactly to continue:`
);
if (entered !== server.name) return;
await api(`/api/servers/${id}`, { method: 'DELETE' });
```

Prefer a proper accessible confirmation dialog and a new API with separate `detach`, `delete_managed_data`, and `delete_backups` choices rather than expanding a misleading browser prompt forever. Coordinate this with the durable deletion protocol in A21.

**Regression:** Browser tests must verify that the confirmation accurately distinguishes managed data, adopted external data, and panel backups; cancellation must make no destructive request.

<a id="a36"></a>

### A36 — High — Concurrent plugin installation or toggling can overwrite a file despite the no-overwrite promise

**Evidence:** Installation checks for existing `.jar`/`.jar.disabled` before a download that may last five minutes, then publishes through `writeAtomicIn`, whose rename replaces the target. Two installers can both pass the check and the last rename wins. Toggling likewise checks absence and then uses replacing rename. Shared `fsMu` does not exclude another installer/toggle. [PLUGINS] [FILES]

**Fix:** Download to a private staged file outside the game tree, validate it, then take the mutation gate for a short, checked publication. Use an atomic no-replace filesystem operation, not stat-then-rename. Go 1.26's Root supports a confined link operation. [OSDOC]

```go
// tmp is a uniquely created, fully written/synced regular file on the same
// filesystem, addressed through root. fsMu/editMu are already held.
func publishNoReplace(root *os.Root, tmp, target string) error {
    if err := root.Link(tmp, target); err != nil {
        return fmt.Errorf("publish without replacement: %w", err)
    }
    // Target exists now. A temp-cleanup failure is not an installation failure.
    if err := root.Remove(tmp); err != nil {
        log.Printf("plugin published; temporary link cleanup failed: %v", err)
    }
    dir, err := root.Open(path.Dir(target))
    if err != nil { return fmt.Errorf("published; durability uncertain: %w", err) }
    if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
        return fmt.Errorf("published; durability uncertain: %w", err)
    }
    return nil
}
```

Under the same edit gate, recheck the sibling enabled/disabled name and server identity. For toggling, use no-replace linking followed by checked source removal for regular files, or a platform no-replace rename. If source removal fails, report the partial state rather than silently implying the toggle completed. For hostile external writers, use the platform's atomic no-replace rename and reject symlink sources; the panel mutex alone does not control them.

**Regression:** Race two same-name downloads, installation against a toggle, and a destination created during download. Existing bytes must never be silently replaced; errors must accurately describe whether publication occurred.

<a id="a37"></a>

### A37 — Medium — Plugin download validation accepts a four-byte signature rather than an intact executable archive

**Evidence:** Plugin installation accepts HTTP as well as HTTPS and verifies only that bytes start with the ZIP local-header magic. A truncated or invalid ZIP with that prefix passes. No expected digest or publisher identity is required before the jar is installed for future execution. HTTP/private-network downloads are an explicit operator-trust policy, not an accidental unauthenticated SSRF finding. [PLUGINS]

**Fix:** Default to HTTPS, allow exceptions only by explicit deployment policy, verify a valid bounded ZIP/JAR including entry CRCs, and support a trusted expected SHA-256 digest. Digest verification establishes integrity against a trusted supplied digest, not trust in an arbitrary publisher.

```go
func validateJar(data []byte, expectedSHA256 string) error {
    if expectedSHA256 != "" {
        expected, err := hex.DecodeString(expectedSHA256)
        actual := sha256.Sum256(data)
        if err != nil || len(expected) != len(actual) ||
            subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
            return fmt.Errorf("jar SHA-256 mismatch")
        }
    }
    zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
    if err != nil { return fmt.Errorf("invalid jar archive: %w", err) }
    if len(zr.File) == 0 || len(zr.File) > 50000 { return fmt.Errorf("invalid jar entry count") }
    remaining := int64(512 << 20) // A verification budget, not an install-size limit.
    for _, entry := range zr.File {
        if entry.FileInfo().IsDir() { continue }
        if entry.UncompressedSize64 > uint64(remaining) { return fmt.Errorf("jar expands beyond validation budget") }
        reader, err := entry.Open()
        if err != nil { return err }
        n, readErr := io.Copy(io.Discard, io.LimitReader(reader, remaining+1))
        closeErr := reader.Close()
        if err := errors.Join(readErr, closeErr); err != nil { return err }
        if n > remaining { return fmt.Errorf("jar expands beyond validation budget") }
        remaining -= n
    }
    return nil
}
```

Require HTTPS on every redirect unless the explicit exception applies. Keep the existing download byte/time caps, add global download admission limits, and do not characterize valid JAR structure as a malware scan or sandbox.

**Regression:** Four-byte-only payload, truncated central directory, bad CRC, decompression bomb, wrong expected digest, and an HTTPS-to-HTTP redirect must have explicit safe outcomes.

<a id="a38"></a>

### A38 — Medium — Plugin installation can succeed after the HTTP response has already timed out

**Evidence:** The HTTP server has a 60-second absolute write timeout; plugin downloads have a five-minute client timeout. Unlike the long backup handlers, the install handler does not adjust this deadline or expose an asynchronous job. Installation can finish after the client receives a transport error, encouraging a retry of work that already took effect. The filesystem gate is held across the download. [APP] [PLUGINS]

**Fix:** Prefer a bounded operation queue with a 202 response, operation ID, idempotency key, and pollable terminal result. An immediate smaller change is to propagate a shorter request context into the download and ensure publication occurs only while that context is live.

```go
// Signature change: InstallPlugin(ctx context.Context, s *Server, rawURL string).
ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
defer cancel()
entry, err := a.mgr.InstallPlugin(ctx, s, body.URL)

// Inside InstallPlugin, replace client.Get:
request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
if err != nil { return PluginEntry{}, err }
response, err := client.Do(request)
if err != nil { return PluginEntry{}, err }
// Download/validate before taking the filesystem mutation locks.
// Immediately before publication, under those locks:
if err := ctx.Err(); err != nil { return PluginEntry{}, err }
```

A timeout budget cannot bound an indefinitely stalled local filesystem write, and cancellation can race the final commit. Return/reconcile an operation result rather than asserting that every transport failure means “not installed.” For legitimately slow downloads, implement the operation resource rather than removing all connection deadlines.

**Regression:** Delay the remote response beyond the synchronous budget and verify no late untracked installation. For queued jobs, disconnect/retry the initiating client and verify idempotent operation lookup and one publication.

<a id="a39"></a>

### A39 — Medium — Release publication is not gated by the repository tests

**Evidence:** CI runs formatting, vet, Go race tests, and the frontend routing test on main pushes/pull requests. The tag-triggered release workflow independently publishes binaries and an image; neither publishing job depends on a test job. A tag can therefore publish a commit that has not passed those checks. [CI] [RELEASE]

**Fix implementation:** Make verification reusable and require it for both publishing jobs. This adds a test dependency without confusing main-branch CI status with verification of the exact tagged commit.

```yaml
# .github/workflows/ci.yml — retain the existing test job.
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
  workflow_call:
permissions:
  contents: read
```

```yaml
# .github/workflows/release.yml — integration additions.
jobs:
  verify:
    uses: ./.github/workflows/ci.yml
    permissions:
      contents: read
  release:
    needs: verify
    # retain release job's reviewed steps and least-required permissions
  image:
    needs: verify
    # retain image job's reviewed steps and least-required permissions
```

Also publish `latest` only for stable releases: the existing unconditional `type=raw,value=latest` can move that tag on a prerelease. Validate version tags before publishing and attach commit/version metadata consistently to binary and container outputs.

**Regression:** Tag a deliberately failing test commit in a disposable repository/workflow test; neither publishing job may execute. A prerelease must not replace the stable latest image tag.

<a id="a40"></a>

### A40 — Low — Build reproducibility and regression coverage do not yet match the failure modes of this service

**Evidence:** Release automation uses mutable action major-version tags and `goreleaser` version `latest`; Docker base images are tag-based. Frontend CI runs the routing test, not a browser workflow. The reviewed files contain failure modes requiring crash-point, persistence-fault, stream, and concurrent-admission tests beyond ordinary happy-path unit coverage. Existing Go race testing is valuable and should be retained. [RELEASE] [DOCKER] [CI]

**Classification:** Hardening/testing gap. This audit did not establish a specific vulnerable dependency version and did not run a dependency vulnerability scan. Do not treat this item as evidence of a supply-chain compromise.

**Fix implementation:** Pin reviewed action revisions, the release tool version, and container base digests; update them through reviewed automated dependency changes. Add the probe-derived regression cases and browser/system tests to CI.

```yaml
# Add to the verification job after Go setup; use a reviewed pinned tool version.
- name: Verify module checksums
  run: go mod verify
- name: Repeat concurrency-sensitive tests
  run: go test -race -count=20 ./internal/arcade
- name: Verify frontend JavaScript syntax
  run: |
    for file in cmd/teploy-arcade/frontend/*.js; do
      node --check "$file"
    done
```

```sh
# Resolve a chosen release to an immutable revision during dependency review,
# then commit that reviewed SHA to uses: (do not paste an invented SHA).
git ls-remote https://github.com/actions/checkout.git 'refs/tags/v4*'
# Similarly choose a reviewed GoReleaser release and image digest; replace
# version: latest and tag-only base references with those exact reviewed values.
```

Add a pinned `govulncheck` run and a real-browser smoke suite covering setup, login expiry, restore/deletion confirmation, failed mutations, and SSE/WebSocket reconnection. The commands above are runnable additions, not a claim that a complete browser suite or supply-chain attestation pipeline is supplied here. Protect release tags/branches with required verification appropriate to the repository's governance.

**Regression:** Rebuild an identical release from identical pinned inputs, verify version/commit metadata, and ensure each high-impact finding has a failing-before/passing-after test rather than a test of a copied implementation.

<a id="a41"></a>

### A41 — Medium — Failed archive assembly leaves partial files behind and reuses a predictable temporary name

**Evidence:** `tarGz` opens `dst + ".part"` with create/truncate, not exclusive creation. Errors from the tree walk or tar/gzip close return before the later cleanup branches. The caller removes `dst`, not the partial filename. Repeated failed backups can therefore retain partial archives and consume the shared disk until a later sweep. [BACKUP]

**Fix:** Use a unique same-directory temporary file, defer cleanup immediately after successful creation, and publish with the already-required no-replace operation. Cleanup must not delete an existing final archive that this attempt did not create.

```go
f, err := os.CreateTemp(filepath.Dir(dst), ".arcade-tmp-backup-*")
if err != nil { return 0, err }
part := f.Name()
defer func() { _ = f.Close(); _ = os.Remove(part) }()
// Write tar and gzip; check every write, both format closes, f.Sync and f.Close.
// Only a completed, synced archive is published:
if err := os.Link(part, dst); err != nil { return 0, err }
// Sync the destination directory and classify post-publication errors honestly.
```

Remove the caller's unconditional removal of the final destination on an assembly error. Use a flag/result that states whether this attempt actually published it. Keep the temporary prefix aligned with the narrow boot-sweep policy.

**Regression:** Fail while walking, reading a source file, closing tar/gzip, syncing, and linking. No partial file should accumulate and a pre-existing final archive must remain unchanged.

<a id="a42"></a>

### A42 — Medium — Creation validates supplied resource values before defaults, and silently converts unknown runtimes to simulator

**Evidence:** `Create` checks host fit using caller-supplied memory/CPU values before constructing the template-derived server. Zero values mean “use defaults,” so the template's resulting memory allocation does not pass that same host-fit check. Default CPU is explicitly clamped later, but memory is not rechecked there. Any runtime string other than `docker` becomes `sim`, including misspellings. [MANAGER]

**Fix:** Normalize defaults first, validate the complete effective configuration, and reject unknown explicit enum values before reserving ports or creating files.

```go
if runtime == "" { runtime = RuntimeSim }
if runtime != RuntimeSim && runtime != RuntimeDocker {
    return nil, fmt.Errorf("unknown runtime %q", runtime)
}
s := m.newServer(name, t, version, port, runtime)
if memMB > 0 { s.MemoryMB = memMB }
if cpu > 0 { s.CPU = cpu }
if cpu == 0 && hostCPUs > 0 && s.CPU > hostCPUs { s.CPU = hostCPUs }
if err := checkServerLimits(s.Port, s.MemoryMB, s.CPU); err != nil { return nil, err }
if err := checkFitsHost(s.MemoryMB, s.CPU); err != nil { return nil, err }
// Only now reserve/admit and materialize the server.
```

Retain the existing checks that reject negative supplied values; normalizing defaults must not turn invalid explicit values into accepted defaults. Avoid reporting an unknown/zero host capacity as proof a configuration fits; document that such admission is unverified or require an override.

**Regression:** Use a small host and a template whose default memory exceeds it, request zero/default memory, and assert a clear refusal before filesystem changes. A misspelled runtime must not report successful creation of a simulated server.

<a id="a43"></a>

### A43 — Low — NextFreePort can recommend an occupied or out-of-range port and ignores candidate geometry

**Evidence:** `NextFreePort` searches only 400 values, does not cap candidates at 65535, compares a single base number, and returns the original hint when no candidate is found. Creation later checks full template geometry, so an apparently free suggested base can fail because its span/extra ports conflict. The downstream candidate validation prevents some invalid starts, but does not make the recommendation correct. [MANAGER]

**Fix:** Return an error on exhaustion and evaluate the actual requested binding set. Keep suggestion separate from atomic admission; another operation may claim a suggested port before creation.

```go
func nextPortCandidate(hint, span int, protocols, extras []string,
    occupied []portBinding, reserved map[int]string) (int, error) {
    if hint < 1 { hint = 25565 }
    for base := hint; base <= 65535; base++ {
        candidate, err := candidateBindings(base, protocols, span, extras)
        if err != nil { continue }
        if bindingConflict(occupied, candidate) { continue }
        if reservedConflicts(reserved, candidate) != "" { continue }
        return base, nil
    }
    return 0, fmt.Errorf("no compatible port binding is available")
}
```

Take a consistent snapshot under the appropriate manager/server locks, pass the template geometry, and keep the final lease claim atomic as in R2. A fixed extra port already occupied means no base is compatible; detect that early rather than scanning unnecessarily.

**Regression:** Exhaust the search range, request a hint at 65535 with a multi-port span, and occupy a candidate's fixed extra port. The API must return a useful refusal, never an occupied fallback or invalid port number.

## Shared implementation R1 — Journaled, restartable filesystem restore recovery

**Addresses A02/A03. Implementation status:** The following recovery core and durable-journal writer were executed in an isolated Go module with a temporary-directory filesystem adapter. Eight focused tests passed. They were **not** integrated into or compiled with the repository. In production, pass the registered server's `*os.Root` (Go 1.26) as `restoreFS`; the test adapter below is not a confinement implementation. [OSDOC]

Create `internal/arcade/restore_transaction.go`, adapting names to avoid duplicates with the phase definitions shown in A02. Keep only one copy of each type/helper. Initialize a service-owned `0700` journal directory outside all server/adopted trees. Read and validate its records at boot, resolve each record against a registered server, and open that server's root. Do not discover authoritative transactions merely by scanning game-writable directory prefixes.

```go
package arcade

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type restorePhase string

const (
	phaseEvacuating restorePhase = "evacuating"
	phaseInstalling restorePhase = "installing"
	phaseRemoveNew  restorePhase = "rollback-remove-new"
	phaseRestoreOld restorePhase = "rollback-restore-old"
	phaseCommitted  restorePhase = "committed"
	phaseRolledBack restorePhase = "rolled-back"
)

type restoreJournal struct {
	Version  int          `json:"version"`
	ServerID string       `json:"server_id"`
	TxID     string       `json:"tx_id"`
	Phase    restorePhase `json:"phase"`
	OldNames []string     `json:"old_names"`
	NewNames []string     `json:"new_names"`
}

// *os.Root in Go 1.26 implements this interface. Keeping the interface small
// also permits fault-injection tests without a running game or Docker daemon.
type restoreFS interface {
	Lstat(string) (os.FileInfo, error)
	Rename(string, string) error
	RemoveAll(string) error
	Open(string) (*os.File, error)
}
type restoreJournalStore interface{ Write(restoreJournal) error }

func bareRestoreName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") &&
		!strings.HasPrefix(name, ".arcade-restore-")
}
func validateRestoreJournal(j restoreJournal, staging string) error {
	if j.Version != 1 || !bareRestoreName(j.ServerID) || len(j.TxID) != 32 {
		return fmt.Errorf("invalid restore journal identity")
	}
	for _, c := range j.TxID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("invalid transaction ID")
		}
	}
	if staging != ".arcade-restore-"+j.TxID {
		return fmt.Errorf("staging identity mismatch")
	}
	for _, names := range [][]string{j.OldNames, j.NewNames} {
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if !bareRestoreName(name) || seen[name] {
				return fmt.Errorf("invalid or duplicate transaction entry %q", name)
			}
			seen[name] = true
		}
	}
	switch j.Phase {
	case phaseEvacuating, phaseInstalling, phaseRemoveNew, phaseRestoreOld,
		phaseCommitted, phaseRolledBack:
		return nil
	default:
		return fmt.Errorf("unknown restore phase %q", j.Phase)
	}
}
func syncRestoreDir(root restoreFS, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func setRestorePhase(store restoreJournalStore, j *restoreJournal, next restorePhase) error {
	candidate := *j
	candidate.Phase = next
	if err := store.Write(candidate); err != nil {
		return err
	}
	*j = candidate
	return nil
}

// recoverRestore rolls back an uncommitted transaction. It NEVER deletes the
// staging tree or journal; the caller may clean only after a terminal phase.
// The authoritative journal is loaded from a private service-owned directory,
// validated against the registered server, and passed with that server's Root.
// All game writers must be stopped and the server filesystem gate held.
func recoverRestore(store restoreJournalStore, root restoreFS, staging string,
	j *restoreJournal) error {
	if err := validateRestoreJournal(*j, staging); err != nil {
		return err
	}
	switch j.Phase {
	case phaseCommitted, phaseRolledBack:
		return nil
	case phaseEvacuating:
		// Some original entries may still be live. Remove NONE of them.
		if err := setRestorePhase(store, j, phaseRestoreOld); err != nil {
			return err
		}
	case phaseInstalling:
		if err := setRestorePhase(store, j, phaseRemoveNew); err != nil {
			return err
		}
	}
	if j.Phase == phaseRemoveNew {
		// Original evacuation was durably completed before installation began.
		// Repeating these removals is safe because restoration has not started.
		for _, name := range j.NewNames {
			if err := root.RemoveAll(name); err != nil {
				return fmt.Errorf("remove replacement %q: %w", name, err)
			}
		}
		if err := syncRestoreDir(root, "."); err != nil {
			return err
		}
		// Persist BEFORE the first original returns. A second crash must not
		// cause recovery to remove a restored original with a replacement name.
		if err := setRestorePhase(store, j, phaseRestoreOld); err != nil {
			return err
		}
	}
	if j.Phase != phaseRestoreOld {
		return fmt.Errorf("unexpected recovery phase")
	}
	held := path.Join(staging, "old")
	for _, name := range j.OldNames {
		heldName := path.Join(held, name)
		_, heldErr := root.Lstat(heldName)
		if errors.Is(heldErr, os.ErrNotExist) {
			// Never moved, or restored before this recovery attempt.
			if _, err := root.Lstat(name); err != nil {
				return fmt.Errorf("original %q is missing from both locations: %w", name, err)
			}
			continue
		}
		if heldErr != nil {
			return heldErr
		}
		if _, err := root.Lstat(name); err == nil {
			return fmt.Errorf("restore collision at %q; original retained in staging", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := root.Rename(heldName, name); err != nil {
			return err
		}
		if err := syncRestoreDir(root, held); err != nil {
			return err
		}
		if err := syncRestoreDir(root, "."); err != nil {
			return err
		}
	}
	return setRestorePhase(store, j, phaseRolledBack)
}

// The directory must be initialized as service-owned 0700, outside every
// game-writable/adopted tree. The filename is derived from validated IDs only.
type diskRestoreStore struct{ dir string }

func (store diskRestoreStore) Write(j restoreJournal) error {
	if err := validateRestoreJournal(j, ".arcade-restore-"+j.TxID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(store.dir, ".arcade-tmp-journal-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	target := filepath.Join(store.dir, j.ServerID+"-"+j.TxID+".json")
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	dir, err := os.Open(store.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

```

### Required integration ordering

At restore admission, require a confirmed non-writing game state, hold the per-server filesystem gate, validate the extracted archive and all model changes, and generate a unique transaction ID. Retain before/after **model** snapshots and reserve both old and prospective port-binding sets until commit. The model snapshot must include every value the restore changes, not just the port.

Before the first original entry moves, record `phaseEvacuating` with the complete original and replacement top-level name lists. Move each original entry into `old/`, check every error, and sync the affected directories. Only after evacuation is durable, record `phaseInstalling`. Install replacements, verify ownership and file/directory durability, and persist the corresponding model through a checked transaction. Only then record `phaseCommitted` and clean up.

On any pre-commit failure, call `recoverRestore`. It deliberately retains all recovery artifacts. Once filesystem rollback reaches `phaseRolledBack`, restore and persist the **before-model** snapshot and reconcile binding reservations before allowing cleanup or startup. After a committed transaction, reconcile the **after-model** snapshot. If model repair, journal persistence, or cleanup fails, retain the journal and block operations for that server with an actionable error. A terminal filesystem phase alone does not certify complete multi-resource recovery.

For cleanup, remove staging only after the matching committed/full-rollback model outcome is confirmed, sync the server root, remove the private journal, and sync its parent. Repeating cleanup should be safe. A post-rename sync error is an uncertain outcome requiring reconciliation, not permission to delete recovery evidence.

This protocol assumes the underlying filesystem implements the requested durability semantics. Fault-injection and real filesystem/power-loss tests are still required before relying on it for production recovery. The core does not solve concurrent external game writes; those must be excluded by snapshot admission and lifecycle policy.

### Regression tests for the recovery core

These are the eight tests actually run against the proposed core. They cover partial evacuation, partial installation, repeated rollback after a second failure, an empty original tree, collision preservation, journal failure, untrusted names, and journal-file publication/permissions. They do not simulate real power loss, Docker, or `os.Root` confinement.

```go
package arcade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type localRestoreFS struct {
	dir          string
	renames      int
	failRenameAt int
}

func (f *localRestoreFS) p(s string) string                   { return filepath.Join(f.dir, filepath.FromSlash(s)) }
func (f *localRestoreFS) Lstat(s string) (os.FileInfo, error) { return os.Lstat(f.p(s)) }
func (f *localRestoreFS) Open(s string) (*os.File, error)     { return os.Open(f.p(s)) }
func (f *localRestoreFS) RemoveAll(s string) error            { return os.RemoveAll(f.p(s)) }
func (f *localRestoreFS) Rename(a, b string) error {
	f.renames++
	if f.failRenameAt == f.renames {
		return errors.New("injected rename failure")
	}
	return os.Rename(f.p(a), f.p(b))
}

type memoryRestoreStore struct {
	phases []restorePhase
	fail   bool
}

func (s *memoryRestoreStore) Write(j restoreJournal) error {
	if s.fail {
		return errors.New("injected journal failure")
	}
	s.phases = append(s.phases, j.Phase)
	return nil
}

const testTx = "0123456789abcdef0123456789abcdef"

func fixture(t *testing.T, phase restorePhase, old, fresh []string, files map[string]string) (*localRestoreFS, *memoryRestoreStore, *restoreJournal, string) {
	t.Helper()
	f := &localRestoreFS{dir: t.TempDir()}
	stage := ".arcade-restore-" + testTx
	for _, d := range []string{stage + "/old", stage + "/new"} {
		if err := os.MkdirAll(f.p(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(f.p(name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.p(name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f, &memoryRestoreStore{}, &restoreJournal{Version: 1, ServerID: "server1", TxID: testTx, Phase: phase, OldNames: old, NewNames: fresh}, stage
}
func assertBytes(t *testing.T, f *localRestoreFS, name, want string) {
	t.Helper()
	b, e := os.ReadFile(f.p(name))
	if e != nil || string(b) != want {
		t.Fatalf("%s: %q %v; want %q", name, b, e, want)
	}
}
func TestRecoveryCorePartialEvacuation(t *testing.T) {
	stage := ".arcade-restore-" + testTx
	f, s, j, p := fixture(t, phaseEvacuating, []string{"a", "b"}, []string{"a", "c"}, map[string]string{stage + "/old/a": "old-a", "b": "old-b"})
	if err := recoverRestore(s, f, p, j); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, f, "a", "old-a")
	assertBytes(t, f, "b", "old-b")
	if j.Phase != phaseRolledBack {
		t.Fatal(j.Phase)
	}
}
func TestRecoveryCorePartialInstallation(t *testing.T) {
	stage := ".arcade-restore-" + testTx
	f, s, j, p := fixture(t, phaseInstalling, []string{"a", "b"}, []string{"a", "c"}, map[string]string{stage + "/old/a": "old-a", stage + "/old/b": "old-b", "a": "new-a", "c": "new-c"})
	if err := recoverRestore(s, f, p, j); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, f, "a", "old-a")
	assertBytes(t, f, "b", "old-b")
	if _, e := os.Stat(f.p("c")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("replacement c remains")
	}
}
func TestRecoveryCoreSecondCrashDuringRollback(t *testing.T) {
	stage := ".arcade-restore-" + testTx
	f, s, j, p := fixture(t, phaseInstalling, []string{"a", "b"}, []string{"a", "c"}, map[string]string{stage + "/old/a": "old-a", stage + "/old/b": "old-b", "a": "new-a", "c": "new-c"})
	f.failRenameAt = 2
	if err := recoverRestore(s, f, p, j); err == nil {
		t.Fatal("expected injected failure")
	}
	if j.Phase != phaseRestoreOld {
		t.Fatal("rollback restoration phase was not durable first")
	}
	assertBytes(t, f, "a", "old-a")
	assertBytes(t, f, stage+"/old/b", "old-b")
	f.failRenameAt = 0
	if err := recoverRestore(s, f, p, j); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, f, "a", "old-a")
	assertBytes(t, f, "b", "old-b")
}
func TestRecoveryCoreEmptyOriginal(t *testing.T) {
	f, s, j, p := fixture(t, phaseInstalling, nil, []string{"c"}, map[string]string{"c": "new-c"})
	if err := recoverRestore(s, f, p, j); err != nil {
		t.Fatal(err)
	}
	if _, e := os.Stat(f.p("c")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("replacement remains")
	}
}
func TestRecoveryCoreCollisionPreservesHeld(t *testing.T) {
	stage := ".arcade-restore-" + testTx
	f, s, j, p := fixture(t, phaseRestoreOld, []string{"a"}, []string{"a"}, map[string]string{stage + "/old/a": "old-a", "a": "unexpected"})
	if err := recoverRestore(s, f, p, j); err == nil {
		t.Fatal("expected collision error")
	}
	assertBytes(t, f, stage+"/old/a", "old-a")
	assertBytes(t, f, "a", "unexpected")
}
func TestRecoveryCoreJournalFailureMakesNoMutation(t *testing.T) {
	stage := ".arcade-restore-" + testTx
	f, s, j, p := fixture(t, phaseInstalling, []string{"a"}, []string{"a"}, map[string]string{stage + "/old/a": "old-a", "a": "new-a"})
	s.fail = true
	if err := recoverRestore(s, f, p, j); err == nil {
		t.Fatal("expected journal error")
	}
	assertBytes(t, f, stage+"/old/a", "old-a")
	assertBytes(t, f, "a", "new-a")
}
func TestRecoveryCoreRejectsUntrustedNames(t *testing.T) {
	f, s, j, p := fixture(t, phaseInstalling, nil, []string{"../outside"}, nil)
	if err := recoverRestore(s, f, p, j); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatal("expected validation error", err)
	}
}
func TestDiskRestoreJournalWrite(t *testing.T) {
	store := diskRestoreStore{dir: t.TempDir()}
	j := restoreJournal{Version: 1, ServerID: "server1", TxID: testTx, Phase: phaseEvacuating}
	if err := store.Write(j); err != nil {
		t.Fatal(err)
	}
	j.Phase = phaseInstalling
	if err := store.Write(j); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("unexpected temp files", len(entries))
	}
	st, err := os.Stat(filepath.Join(store.dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatal("journal mode", st.Mode())
	}
}

```

## Shared implementation R2 — Unique leases and complete port-reservation release

**Addresses A19; coordinate with A20. Implementation status:** Proposed repository integration, not executed against the repository.

```go
// manager.go. Called with m.mu held.
func (m *Manager) releaseReservationLocked(leaseID string) {
    for _, port := range m.reservedByWho[leaseID] {
        // Never release a port whose ownership has changed.
        if m.reservedPorts[port] == leaseID {
            delete(m.reservedPorts, port)
        }
    }
    delete(m.reservedByWho, leaseID)
}

func (m *Manager) releaseReservation(leaseID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.releaseReservationLocked(leaseID)
}
```

For **every** create/clone/import admission, generate `leaseID` with `randomHex(16)` and pass it to `claimPortBindings`. Place `defer m.releaseReservation(leaseID)` immediately after a successful claim. Keep a separate display label for diagnostics. Before transferring the claim, construct the new server so its binding geometry represents every reserved span/extra port.

Inside the same `m.mu` critical section that inserts the provisional server, call `releaseReservationLocked(leaseID)`. The registered object now owns the binding set, so there must be no gap between removing a reservation and adding its owner. Keep lifecycle exclusion through checked persistence, as the existing create path intends. On persistence failure, remove the provisional registration and its files, but never recreate a leaked reservation. The deferred lease cleanup is idempotent.

Remove base-port-derived release calls after conversion; those cannot reliably identify a lease once the base mapping has been removed. Make admission compare server bindings with their own mutex held (A20), and preserve the manager/server lock order.

```go
// Create integration (same pattern applies to clone/import):
leaseID, err := randomHex(16)
if err != nil { return nil, err }
if holder, ok := m.claimPortBindings(cand, leaseID); !ok {
    return nil, fmt.Errorf("requested bindings conflict with %s", holder)
}
defer m.releaseReservation(leaseID)

// Seed/validate s without exposing it. Then:
m.lifecycle.Lock()
m.mu.Lock()
m.servers[s.ID] = s
m.order = append(m.order, s.ID)
m.releaseReservationLocked(leaseID)
m.mu.Unlock()
// Keep the existing checked Save and rollback while lifecycle remains held.
// All success/error paths must unlock lifecycle exactly once.
```

Required tests must inspect both `reservedPorts` and `reservedByWho`: no residue after success, failure, or cancellation; no cross-release between concurrent same-name operations; and atomic admission of complete spans/extras.

## Shared implementation R3 — Strict, bounded JSON input decoding

**Supports A11/A13/A14/A29. Implementation status:** Proposed helper; migrate route by route, retaining endpoint-specific validation. Required imports: `encoding/json`, `errors`, `fmt`, `io`, `mime`, `net/http`.

```go
func decodeJSONRequest(w http.ResponseWriter, r *http.Request,
    destination any, maximum int64, optional bool) error {
    if optional && r.ContentLength == 0 &&
        (r.Body == nil || r.Body == http.NoBody) {
        return nil
    }
    mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
    if err != nil || mediaType != "application/json" {
        return fmt.Errorf("Content-Type must be application/json")
    }
    r.Body = http.MaxBytesReader(w, r.Body, maximum)
    decoder := json.NewDecoder(r.Body)
    decoder.DisallowUnknownFields()
    if err := decoder.Decode(destination); err != nil {
        if optional && errors.Is(err, io.EOF) { return nil }
        return err
    }
    var extra any
    if err := decoder.Decode(&extra); err != io.EOF {
        if err != nil { return err }
        return fmt.Errorf("only one JSON value is allowed")
    }
    return nil
}
```

Use a small authentication limit (for example 16 KiB), a bounded backup-note request, and the existing file-upload limit only where genuinely needed. Map invalid media type to 415, oversized bodies to 413, and schema/validation errors to 400 with typed errors rather than matching error strings. Decide explicitly whether empty and JSON `null` are legal per route. This helper does not replace a login rate/concurrency limiter or transaction-level validation.

## Validation performed and limits

The following were performed locally against an **isolated** module, not the repository:

| Check | Observed result | Meaning |
|---|---|---|
| Tar EOF versus corrupt gzip trailer | Reproduced | Tar completion can precede gzip checksum validation. |
| Partial original-file evacuation followed by current recovery behavior | Reproduced | An original entry that never moved was deleted. |
| Midnight plus elapsed duration on Vancouver's 2026 spring-forward day | Reproduced | Requested civil 03:30 was displayed as 04:30. |
| JSON pointer list containing null | Reproduced | Successful JSON decoding still produces a nil pointer element. |
| Fresh avatar preference expression | Evaluated true | `null !== 'off'` enables external avatars by default. |
| Proposed R1 recovery core | Eight tests passed | The tested algorithm handled the covered temporary-directory cases. |

Toolchain used for these isolated checks: **Go 1.23.2**; frontend expression checked with **Node 22.16.0**. The repository requires **Go 1.26** and uses newer APIs; its full `go test`, `go vet`, race suite, build, Docker runtime, and browser workflows were **not run during this audit**. Direct network checkout/toolchain acquisition was unavailable in the local execution environment; source inspection was through GitHub's connected file-reading capability. The repository's earlier audit log reports tests from earlier work, but those reports are not this audit's test results. [GOMOD] [AUDITOPEN]

A Go probe that intentionally reproduces a bug reports PASS when it observes the bug. Do not misread the first four PASS results below as evidence the repository is fixed. The eight R1 tests instead assert the proposed recovery behavior.

### Reproduction code actually executed

Save this in a **separate scratch module**, not in the production repository unchanged. The filesystem probe intentionally performs destructive recovery steps only inside a temporary test directory. It mirrors the relevant algorithm; it does not import the repository.

```go
package probes

import (
 "archive/tar"
 "bytes"
 "compress/gzip"
 "encoding/json"
 "io"
 "os"
 "path/filepath"
 "testing"
 "time"
)

func TestTarEOFCanHideGzipChecksumFailure(t *testing.T) {
 var raw bytes.Buffer
 tw := tar.NewWriter(&raw)
 if err:=tw.WriteHeader(&tar.Header{Name:"world.txt",Mode:0600,Size:3});err!=nil {t.Fatal(err)}
 if _,err:=tw.Write([]byte("old"));err!=nil {t.Fatal(err)}
 if err:=tw.Close();err!=nil {t.Fatal(err)}
 var packed bytes.Buffer
 zw:=gzip.NewWriter(&packed)
 if _,err:=zw.Write(raw.Bytes());err!=nil {t.Fatal(err)}
 if err:=zw.Close();err!=nil {t.Fatal(err)}
 b:=packed.Bytes(); b[len(b)-8]^=0xff
 zr,err:=gzip.NewReader(bytes.NewReader(b));if err!=nil{t.Fatal(err)}
 tr:=tar.NewReader(zr)
 if _,err=tr.Next();err!=nil{t.Fatal(err)}
 if _,err=io.Copy(io.Discard,tr);err!=nil{t.Fatal(err)}
 if _,err=tr.Next();err!=io.EOF{t.Fatalf("expected tar EOF, got %v",err)}
 if _,err=io.Copy(io.Discard,zr);err!=gzip.ErrChecksum{t.Fatalf("expected hidden checksum error, got %v",err)}
 t.Log("tar EOF was reached before the gzip checksum failure was observed")
}

func TestPartialEvacuationRecoveryLosesUnmovedOriginal(t *testing.T) {
 dir:=t.TempDir(); stage:=filepath.Join(dir,".arcade-restore-probe"); held:=filepath.Join(stage,"old")
 if err:=os.MkdirAll(held,0700);err!=nil{t.Fatal(err)}
 for _,name:=range []string{"a.txt","b.txt"} {if err:=os.WriteFile(filepath.Join(dir,name),[]byte(name),0600);err!=nil{t.Fatal(err)}}
 if err:=os.Rename(filepath.Join(dir,"a.txt"),filepath.Join(held,"a.txt"));err!=nil{t.Fatal(err)}
 // Reproduce the non-empty old/ recovery branch at the audited snapshot.
 ents,err:=os.ReadDir(dir);if err!=nil{t.Fatal(err)}
 for _,e:=range ents {if e.Name()!=filepath.Base(stage) {if err:=os.RemoveAll(filepath.Join(dir,e.Name()));err!=nil{t.Fatal(err)}}}
 ents,err=os.ReadDir(held);if err!=nil{t.Fatal(err)}
 for _,e:=range ents {if err:=os.Rename(filepath.Join(held,e.Name()),filepath.Join(dir,e.Name()));err!=nil{t.Fatal(err)}}
 if _,err:=os.Stat(filepath.Join(dir,"b.txt"));!os.IsNotExist(err){t.Fatalf("expected loss of unmoved original, got %v",err)}
 t.Log("b.txt was deleted even though it had never been moved into old/")
}

func TestMidnightPlusDurationIsNotWallClockOnDSTDay(t *testing.T) {
 loc,err:=time.LoadLocation("America/Vancouver");if err!=nil{t.Fatal(err)}
 midnight:=time.Date(2026,time.March,8,0,0,0,0,loc)
 got:=midnight.Add(3*time.Hour+30*time.Minute)
 if got.Hour()!=4 || got.Minute()!=30 {t.Fatalf("unexpected result: %v",got)}
 t.Logf("03:30 task is displayed as %s by midnight.Add",got.Format(time.RFC3339))
}

func TestJSONNullListElementIsAccepted(t *testing.T) {
 type user struct{Name string}
 var list []*user
 if err:=json.Unmarshal([]byte(`[null]`),&list);err!=nil{t.Fatal(err)}
 if len(list)!=1 || list[0]!=nil{t.Fatal("unexpected parse result")}
 t.Log("successful Unmarshal does not make pointer elements safe to dereference")
}

```

The proposed R1 file/tests above were also placed in that scratch module under the same `probes` package name. The code shown above uses `package arcade` only to indicate its intended integration location. The final run was:

```sh
GOTOOLCHAIN=local go test -v ./...
```

```text
=== RUN   TestTarEOFCanHideGzipChecksumFailure
    probe_test.go:32: tar EOF was reached before the gzip checksum failure was observed
--- PASS: TestTarEOFCanHideGzipChecksumFailure (0.00s)
=== RUN   TestPartialEvacuationRecoveryLosesUnmovedOriginal
    probe_test.go:46: b.txt was deleted even though it had never been moved into old/
--- PASS: TestPartialEvacuationRecoveryLosesUnmovedOriginal (0.00s)
=== RUN   TestMidnightPlusDurationIsNotWallClockOnDSTDay
    probe_test.go:54: 03:30 task is displayed as 2026-03-08T04:30:00-07:00 by midnight.Add
--- PASS: TestMidnightPlusDurationIsNotWallClockOnDSTDay (0.00s)
=== RUN   TestJSONNullListElementIsAccepted
    probe_test.go:62: successful Unmarshal does not make pointer elements safe to dereference
--- PASS: TestJSONNullListElementIsAccepted (0.00s)
=== RUN   TestRecoveryCorePartialEvacuation
--- PASS: TestRecoveryCorePartialEvacuation (0.00s)
=== RUN   TestRecoveryCorePartialInstallation
--- PASS: TestRecoveryCorePartialInstallation (0.00s)
=== RUN   TestRecoveryCoreSecondCrashDuringRollback
--- PASS: TestRecoveryCoreSecondCrashDuringRollback (0.00s)
=== RUN   TestRecoveryCoreEmptyOriginal
--- PASS: TestRecoveryCoreEmptyOriginal (0.00s)
=== RUN   TestRecoveryCoreCollisionPreservesHeld
--- PASS: TestRecoveryCoreCollisionPreservesHeld (0.00s)
=== RUN   TestRecoveryCoreJournalFailureMakesNoMutation
--- PASS: TestRecoveryCoreJournalFailureMakesNoMutation (0.00s)
=== RUN   TestRecoveryCoreRejectsUntrustedNames
--- PASS: TestRecoveryCoreRejectsUntrustedNames (0.00s)
=== RUN   TestDiskRestoreJournalWrite
--- PASS: TestDiskRestoreJournalWrite (0.00s)
PASS
ok  	audit-probes	0.007s
```

The initial scratch-module assembly had a package-name mismatch, corrected before this final run. No production code was changed or tested by that correction.

### Checks still required before merging fixes

Run the repository's Go 1.26 build/vet/race suite, then add real-handler regression tests for every finding. Exercise failure injection at all persistence/restore phases; a handful of unit tests is not a substitute for the crash matrix. Test live Docker daemon disconnections, startup/stop/kill races, adopted directories on another filesystem, file ownership under the actual container UID, slow clients, and browser authentication transitions. Perform a current dependency/image vulnerability scan and verify the exact pinned release inputs.

No live game server was attacked, no privileged game command was sent, no external plugin was installed, and no repository branch/commit/issue was modified. Findings describe the inspected snapshot, not an assertion that a deployed instance was exploited.

## Suggested remediation order

First close the unclaimed-instance operational gate (A01) and ship a fail-closed response to unreadable authentication state (A09). Until restore recovery is replaced and fault-tested, avoid automatic destructive recovery of ambiguous staging directories; retain originals and require explicit operator recovery. Prefer offline-only verified snapshots until live-save acknowledgements are reliable (A02–A08).

Next correct transaction boundaries, complete port leases, server-state locking, deletion durability, and Docker uncertainty handling (A10, A15–A24, A36, A41). These defects can corrupt state or change what a successful operation actually means. Treat restore-model consistency as part of the recovery work, not a follow-up cosmetic change.

Then address credential/stream budgets, secure browser requests, task identity/scheduling/lifecycle, request/job result consistency, privacy/confirmation behavior, and release gating. Add the regression tests with each fix; do not batch all tests after a large rewrite. Re-run the audit against the resulting commit, especially the lock order and error paths introduced by the fixes.

## Source index

All repository references below are pinned to the reviewed commit. Labels used alongside findings link to these actual code files. Function names in each finding identify the reviewed location; a file-level reference does not imply every line of that file was audited.

- **AUTH** — [internal/arcade/auth.go][AUTH]
- **APP** — [internal/arcade/app.go][APP]
- **API** — [internal/arcade/api.go][API]
- **APIEXT** — [internal/arcade/api_ext.go][APIEXT]
- **BACKUP** — [internal/arcade/backup.go][BACKUP]
- **MANAGER** — [internal/arcade/manager.go][MANAGER]
- **FILES** — [internal/arcade/files.go][FILES]
- **RUNNER** — [internal/arcade/runner.go][RUNNER]
- **SCHED** — [internal/arcade/scheduler.go][SCHED]
- **MCP** — [internal/arcade/mcp.go][MCP]
- **PLUGINS** — [internal/arcade/plugins.go][PLUGINS]
- **FRONTEND** — [cmd/teploy-arcade/frontend/app.js][FRONTEND]
- **MAIN** — [cmd/teploy-arcade/main.go][MAIN]
- **DOCKER** — [Dockerfile][DOCKER]
- **CI** — [.github/workflows/ci.yml][CI]
- **RELEASE** — [.github/workflows/release.yml][RELEASE]
- **GOMOD** — [go.mod][GOMOD]
- **AUDITOPEN** — [AUDIT_OPEN.md][AUDITOPEN]
- **OSDOC** — [Go 1.26 os.Root API][OSDOC]
- **HTTPDOC** — [Go 1.26 HTTP origin protection and response controllers][HTTPDOC]
- **GZIPDOC** — [Go 1.26 gzip.Reader integrity/EOF contract][GZIPDOC]
- **EXECDOC** — [Go 1.26 os/exec Cmd output and cancellation contract][EXECDOC]
- **PAPERCOMMANDS** — [Paper documentation: namespaced command configuration][PAPERCOMMANDS]

[AUTH]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/auth.go
[APP]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/app.go
[API]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/api.go
[APIEXT]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/api_ext.go
[BACKUP]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/backup.go
[MANAGER]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/manager.go
[FILES]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/files.go
[RUNNER]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/runner.go
[SCHED]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/scheduler.go
[MCP]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/mcp.go
[PLUGINS]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/internal/arcade/plugins.go
[FRONTEND]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/cmd/teploy-arcade/frontend/app.js
[MAIN]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/cmd/teploy-arcade/main.go
[DOCKER]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/Dockerfile
[CI]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/.github/workflows/ci.yml
[RELEASE]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/.github/workflows/release.yml
[GOMOD]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/go.mod
[AUDITOPEN]: https://github.com/useteploy/teploy-arcade/blob/dcfe671f707bb36fe541690fd27815b37c6435ae/AUDIT_OPEN.md
[OSDOC]: https://pkg.go.dev/os@go1.26.0#Root
[HTTPDOC]: https://pkg.go.dev/net/http@go1.26.0
[GZIPDOC]: https://pkg.go.dev/compress/gzip@go1.26.0#Reader
[EXECDOC]: https://pkg.go.dev/os/exec@go1.26.0#Cmd
[PAPERCOMMANDS]: https://docs.papermc.io/paper/reference/spigot-configuration/
