# Teploy Arcade — repository audit and remediation implementations

**Repository:** [useteploy/teploy-arcade](https://github.com/useteploy/teploy-arcade)  
**Reviewed commit:** [`329e67e73352aa7f972182ee02bd489d73234d8c`](https://github.com/useteploy/teploy-arcade/commit/329e67e73352aa7f972182ee02bd489d73234d8c)  
**Commit time:** September 19, 2026, 09:14:26 UTC  
**Audit date:** September 19, 2026  
**Deliverable:** one Markdown report containing findings, implementation excerpts, shared reference implementations, reproduction sources, test results and verification requirements.

## Executive assessment

The reviewed commit includes substantial prior audit remediation, but several important safety invariants still fail across alternate call paths, restart recovery and real container-image contracts. The highest priorities are **unauthenticated access during specific configuration/setup conditions, preservation of worlds and recovery data, correct interpretation of unknown Docker state, safe Rust RCON defaults, and the release-image metadata failure**. This is not a claim that every installation exposes all these conditions.

The report contains **74 distinct findings/work items: 26 High, 42 Medium and 6 Low**. These are **not 74 newly discovered security vulnerabilities**: they include concrete bugs, conditional security/data-loss risks, independently corroborated items already deferred in the repository, and usability/maintenance improvements. Each entry identifies its evidence class, trigger/impact, source functions/files, suggested implementation and a regression criterion. Related call sites are grouped under shared root causes rather than counted repeatedly.

**Evidence actually obtained:** actual source/configuration files were read from GitHub at the pinned commit; relevant upstream action/image source and primary platform specifications were checked. **23 isolated Go/Node tests passed, plus three Java Properties reference cases.** Of the 23 tests, 17 demonstrate runtime/control-flow behavior or boundary mismatches, and six exercise proposed reference helpers. These are **not the repository's test suite** and are not 23 end-to-end exploit reproductions.

**Important limits:** the environment had Go 1.23.2 while this repository declares Go 1.26. A full checkout/dependency build could not be obtained through the local network path, although GitHub connector source access worked. I did **not** run the full repository tests, a Go 1.26 build, live Docker/game-image tests, a browser suite, a vulnerability scanner, or power-loss/crash-injection testing of the repository. Some exact game-image effects therefore remain contract-based findings whose deployed digest must be verified. Nothing was pushed, changed or executed against the GitHub repository or a live deployment. This is a broad source audit, not a proof that no other bugs exist.

## Reading and implementation guide

Start with the [finding index](#finding-index) and the [repair sequence](#repair-sequence). Each source link is pinned to the reviewed Arcade commit; upstream documentation/action/image references are separately identified where they are not immutable. Function names are supplied instead of invented line ranges.

The code blocks are **proposed implementation excerpts**, not one concatenated patch. Some replace local logic; others introduce types/interfaces/fields that must be wired through the manager, API and persistence model. Imports and repetitive existing handler context are omitted from local excerpts. **H01 is a complete standalone reference file whose six tests were run here.** H02–H08 are integration implementations/contracts; they were not compiled against the repository or tested on its Go 1.26 runtime. H04 explicitly identifies the remaining transaction-driver integration needed; adding only its helper is not a complete crash-consistency fix. Duplicate illustrative definitions across findings/appendices should be consolidated, not pasted twice.

The regression checks on findings are **tests to add/run**, not results claimed to have passed. Only H09's recorded commands/results were executed. Prior audit identifiers mentioned in tracking notes refer to the repository's own audit history; “newly identified” means identified in this review and not matched to the current open register, not a claim of first discovery across every historical report.

### Severity conventions and threat model

**High** means a credible authorization/integrity boundary failure, serious world/recovery loss risk, consequential runtime-policy mismatch, or a release-blocking defect under stated conditions. **Medium** means important correctness, availability, consistency or defense-in-depth work. **Low** means user-facing/operational issues with smaller direct impact. These are contextual priorities, not CVSS scores.

The relevant actors differ: an unauthenticated browser during setup, an authenticated viewer holding read/stream privileges, an operator authorized to edit game files/install code, an administrator, a local game/plugin process able to modify a server directory, and a root-running panel controlling Docker. A finding does not silently grant one actor another's capabilities. A panel that controls the Docker socket has deliberately powerful host authority; the security question is whether a less-trusted input/path/token can cross that boundary unexpectedly.

### Prior improvements retained, not re-reported as absent

The reviewed code already has setup gating, role enforcement, must-change-password controls, password derivation outside the global auth lock in key paths, stronger rooted file operations, archive entry restrictions and ZIP/CRC checks, unique port-lease identities, live/desired binding handling in some paths, serialized save logic, release verification dependencies and added regression tests. Those mechanisms should be retained while completing their coverage. This report does not claim that ordinary authenticated operation is globally unauthenticated, that a viewer can simply bypass backend role checks, that a structurally valid jar is safe code, or that an unsupported guessed dependency CVE applies.

<a id="finding-index"></a>
## Finding index

| ID | Priority | Finding |
|---|---|---|
| [R01](#r01) | High | An empty no-auth host passes the loopback guard but binds all interfaces |
| [R02](#r02) | High | Authentication-mode checks can straddle first-account creation |
| [R03](#r03) | Medium | Creation and password-reset validation disagree with login limits |
| [R04](#r04) | Medium | A login semaphore is not rate limiting, and active session growth is unbounded |
| [R05](#r05) | Medium | Audit entries can amplify memory and serialize authentication on disk I/O |
| [R06](#r06) | High | Cookie-authenticated mutations lack explicit cross-origin protection |
| [R07](#r07) | Medium | Secure session cookies depend on backend TLS rather than the public deployment |
| [R08](#r08) | High | MCP raw console access defeats the advertised narrow capability boundary |
| [R09](#r09) | Medium | The MCP HTTP/JSON-RPC layer accepts malformed or ambiguous requests |
| [R10](#r10) | High | Long-running HTTP work can outlive a failed response without a stable operation identity |
| [R11](#r11) | Medium | WebSocket connections and dead-room joins lack complete admission control |
| [R12](#r12) | Medium | Mutation decoding accepts trailing JSON and silently ignores typo fields |
| [R13](#r13) | High | Recursive ownership repair follows symlinks out of the game tree |
| [R14](#r14) | High | Import and ancillary readers still use check-then-open paths outside os.Root |
| [R15](#r15) | Medium | Directory opens can block on a FIFO before checking that it is a directory |
| [R16](#r16) | High | Registration checks and filesystem locks are not applied uniformly |
| [R17](#r17) | High | State replacement is atomic in name but not durably committed |
| [R18](#r18) | High | Boot recovery can discard staging when reading held originals fails |
| [R19](#r19) | High | Restore lacks a durable transaction connecting the tree, metadata and commit marker |
| [R20](#r20) | High | An incomplete recovery is retained on disk but not represented as a blocked server state |
| [R21](#r21) | High | Deletion destroys data before the registry removal is durable |
| [R22](#r22) | Medium | Startup cleanup still recognizes an overly broad temporary-file suffix |
| [R23](#r23) | High | Settings and resource mutations publish partial state on persistence failure |
| [R24](#r24) | Medium | Port changes can lose restart warnings, and later edits erase earlier warnings |
| [R25](#r25) | High | Managed properties are parsed and emitted differently from Java Properties |
| [R26](#r26) | High | Several settings advertised as immediate are only written to disk |
| [R27](#r27) | High | Live snapshot success is not a verified save-state contract; clone drops resume errors |
| [R28](#r28) | High | Raw console commands can undermine the backup filesystem gate |
| [R29](#r29) | Medium | The tar header can describe a different size than the descriptor being copied |
| [R30](#r30) | High | Independent preflight free-space checks do not reserve shared storage capacity |
| [R31](#r31) | High | String-prefix path checks mishandle roots and sibling prefixes |
| [R32](#r32) | Medium | Import does not enforce all creation invariants or freeze the operator’s selected source |
| [R33](#r33) | Medium | Clone mixes mutable source state with the current template rather than a frozen launch configuration |
| [R34](#r34) | Medium | Import/clone jobs need bounded ownership, durable outcomes and panic cleanup |
| [R35](#r35) | High | Docker transport errors are treated as proof that a container is stopped |
| [R36](#r36) | Medium | Docker subprocess execution is not uniformly time- and output-bounded |
| [R37](#r37) | High | Container re-adoption does not rebuild the live binding ledger from Docker |
| [R38](#r38) | Medium | Port planning is recomputed from a daemon-visible path the panel may not be able to read |
| [R39](#r39) | Medium | Dynamic plugin port discovery is too permissive and its parser is fragile |
| [R40](#r40) | Medium | Server identity and directory allocation are not one atomic uniqueness operation |
| [R41](#r41) | Medium | Log supervision does not reconnect, and process-alive is conflated with readiness |
| [R42](#r42) | Medium | Lifecycle requests need durable desired state and generation fencing |
| [R43](#r43) | Medium | A metrics sampler and several snapshots still expose mutable state without a complete locked copy |
| [R44](#r44) | Medium | CPU percentages are mixed across units, and memory-unit parsing is incomplete |
| [R45](#r45) | Medium | Host capacity and utilization can describe different resource scopes |
| [R46](#r46) | Medium | The minimum Java heap can consume the entire allowed container memory |
| [R47](#r47) | High | The Rust template can expose the upstream image’s known default RCON password |
| [R48](#r48) | High | Rust and Valheim templates do not fully describe their persistent paths and launch settings |
| [R49](#r49) | Medium | Bedrock configures a second listener without declaring the full port geometry |
| [R50](#r50) | Medium | Template seeding can overwrite customized files when its ledger is missing or corrupt |
| [R51](#r51) | Medium | Template validation does not validate the launch contract or catalog uniqueness |
| [R52](#r52) | Medium | Player-list edits can discard fields and do not establish a complete identity contract |
| [R53](#r53) | Low | Player metadata and unanchored log matches can look more authoritative than they are |
| [R54](#r54) | Medium | Due scheduled tasks are dropped when four long jobs occupy the run slots |
| [R55](#r55) | Medium | Scheduler preview, completion bookkeeping and persistence do not define the same occurrence |
| [R56](#r56) | Medium | Task steps have no coherent cancellation and treat lifecycle acceptance as completion |
| [R57](#r57) | Medium | Editing a disabled task re-enables it, and toggle requests overwrite concurrent edits |
| [R58](#r58) | Medium | A slower route request can overwrite a newer navigation |
| [R59](#r59) | Medium | Session expiration and dynamic role changes do not have one client-side state transition |
| [R60](#r60) | Medium | The Kick action inherits chat mode and may broadcast text instead of kicking |
| [R61](#r61) | Medium | Import UI can select adoption implicitly and retain a runtime different from the displayed choice |
| [R62](#r62) | Medium | File and configuration editors have no optimistic concurrency protection |
| [R63](#r63) | Low | The file UI hides the fact that a directory listing was truncated |
| [R64](#r64) | High | Malformed plugin SHA-256 input silently disables verification |
| [R65](#r65) | Medium | Plugin download transport and network access need an explicit trust policy |
| [R66](#r66) | Low | Several small UI affordances claim unavailable behavior or ignore cancellation |
| [R67](#r67) | Low | Keyboard interaction, modal focus and the skip link are incomplete |
| [R68](#r68) | Low | The advertised game address is derived from a listener address and is not IPv6-safe everywhere |
| [R69](#r69) | High | The release metadata expression rejects every image publication |
| [R70](#r70) | Medium | The Alpine runtime base is outside normal scheduled support |
| [R71](#r71) | Medium | Release reproducibility and verification still leave supply-chain/test gaps |
| [R72](#r72) | Medium | Run starts unowned workers before binding and does not provide a complete shutdown contract |
| [R73](#r73) | Medium | Corrupt durable state needs an explicit recovery mode, not silent replacement of the active fleet |
| [R74](#r74) | Low | CLI default-path evaluation has side effects before flags determine whether storage is needed |

## Detailed findings and fixes

## Authentication, API and MCP

<a id="r01"></a>
### R01. An empty no-auth host passes the loopback guard but binds all interfaces

**Priority:** High · **Evidence:** Confirmed; isolated runtime proof  
**Source:** [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go)  
**Relevant code:** isLoopbackHost; Run  
**Relationship to prior work:** Incomplete pass-6 A01 safeguard.

**Problem and impact.** The loopback predicate explicitly accepts the empty string. `Run` subsequently constructs the listener address with `net.JoinHostPort`. An empty host is a wildcard listener, not loopback. An operator invoking the explicit no-auth mode with an empty host can therefore expose the administrative API without authentication. This is conditional on that configuration; it is not a claim that a normal authenticated installation is open. Proof 01 exercised the wildcard-listener behavior locally.

**Suggested fix.** Normalize and validate the actual bind address before creating any workers. Accept only literal loopback addresses in no-auth mode; normalize the convenience name `localhost` to a literal rather than trusting its resolution. Verify the bound listener as defense in depth.

**Implementation excerpt:**

```go
func canonicalNoAuthHost(host string) (string, error) {
    if host == "localhost" { host = "127.0.0.1" }
    ip, err := netip.ParseAddr(host)
    if err != nil || !ip.Unmap().IsLoopback() {
        return "", fmt.Errorf("no-auth requires a literal loopback address")
    }
    return ip.String(), nil
}
// In Run, before net.Listen and before worker startup:
// host, err = canonicalNoAuthHost(host) when noAuth is explicitly enabled.
// After net.Listen:
func verifyNoAuthListener(ln net.Listener) error {
    a, ok := ln.Addr().(*net.TCPAddr)
    if !ok || !a.IP.IsLoopback() { return fmt.Errorf("unsafe no-auth listener: %v", ln.Addr()) }
    return nil
}
```

**Required regression verification.** Table-test empty string, `0.0.0.0`, `::`, non-loopback literals, `localhost`, `127.0.0.2`, and `::1`. Add a real `Run` test proving rejection occurs before any background worker starts.

**Primary external reference:** [Go net.Listen address semantics](https://pkg.go.dev/net#Listen).

<a id="r02"></a>
### R02. Authentication-mode checks can straddle first-account creation

**Priority:** High · **Evidence:** Confirmed control-flow race; isolated interleaving proof  
**Source:** [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go)  
**Relevant code:** Auth.gate; Enabled; SetupRequired; first-user commit  
**Relationship to prior work:** Incomplete pass-6 A01 transition handling.

**Problem and impact.** `gate` first calls `Enabled()` and then, on the false branch, `SetupRequired()`. They are separate observations. A request can see no users before setup commits, then see setup no longer required after the first administrator is installed, and enter the development-mode `next` branch without a session. The vulnerable interval is the first-account transition, not steady-state authorization. Proof 06 models this exact split-observation problem.

**Suggested fix.** Make no-auth an explicit immutable application configuration, not a state inferred from two mutable account queries. Read the account/setup mode under one lock. An empty account set must otherwise always mean setup-gated, including after corrupt-store recovery.

**Implementation excerpt:**

```go
type authMode uint8
const (
    authSetup authMode = iota
    authRequired
    authExplicitLocalOnly
)
// Auth already has forced for explicit --no-auth; configure it only at startup.
func (a *Auth) accessMode() authMode {
    a.mu.RLock()
    defer a.mu.RUnlock()
    if a.forced { return authExplicitLocalOnly }
    if len(a.users) == 0 { return authSetup }
    return authRequired
}
// Replace the two-query branch in gate with ONE switch on accessMode().
// Only authExplicitLocalOnly may call next without a Session.
// authSetup returns 503; authRequired retains role and must-change checks.
```

**Required regression verification.** Use a barrier-controlled first-user commit and concurrent protected requests. No request without a session may receive a protected success on either side of setup. Retain explicit no-auth tests separately.

<a id="r03"></a>
### R03. Creation and password-reset validation disagree with login limits

**Priority:** Medium · **Evidence:** Confirmed; isolated boundary proof  
**Source:** [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)  
**Relevant code:** checkNewUser; SetPassword; setup/admin provisioning; login authInputOK  
**Relationship to prior work:** Incomplete pass-6 A11 credential cap.

**Problem and impact.** Login rejects names longer than 128 bytes and passwords longer than 1,024 bytes, but account creation/password changes do not consistently enforce those upper bounds. A successfully created or reset account can consequently have credentials the login route refuses. Oversized password derivations also bypass the login-only work limiter. Password-length messages say characters while checks count bytes.

**Suggested fix.** Use `CredentialBounds` from implementation H01 for creation, first-run setup, environment provisioning, admin resets, and self-service changes. Keep login failure messages uniform and preserve a deliberate legacy-password migration policy rather than locking out existing accounts unexpectedly.

**Implementation excerpt:**

```go
// At account creation, before salt generation or password hashing:
if err := CredentialBounds(name, password); err != nil { return err }
// At the beginning of SetPassword, validate the proposed password too:
if err := CredentialBounds(name, next); err != nil { return err }
// A separate login validator may allow a legacy short current password,
// but must use the same username/password upper bounds before hashing.
func loginLengthsOK(name, password string) bool {
    return len(name) > 0 && len(name) <= 128 && len(password) <= 1024
}
```

**Required regression verification.** Test upper-bound equality and +1 for every credential ingress, including multibyte Unicode. A credential accepted by creation/reset must be accepted by login's length gate. Test environment bootstrap as well as HTTP.

<a id="r04"></a>
### R04. A login semaphore is not rate limiting, and active session growth is unbounded

**Priority:** Medium · **Evidence:** Confirmed missing controls; hardening  
**Source:** [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)  
**Relevant code:** loginWork; Auth.Login; sessions; reapSessions  
**Relationship to prior work:** Known-open A11; independently corroborated.

**Problem and impact.** The four-slot login semaphore limits concurrent expensive work, not password guesses over time. There is no account/origin attempt budget. Expiration and the session reaper limit session lifetime, but not the number of sessions minted during that lifetime. Password changes and other credential derivations also need a shared CPU-work budget. A session cap must not silently evict an active administrator: the repository explicitly tests that concurrent logins do not disturb live sessions.

**Suggested fix.** Add bounded, expiring IP/account attempt buckets and a global derivation budget, with separate setup and authenticated password-change policies. Reject new sessions once a configurable per-account/global cap is reached; offer explicit session revocation. H05 supplies a bounded fixed-window limiter implementation. Use the connection peer, not untrusted forwarded headers.

**Implementation excerpt:**

```go
// Example admission, before password hashing. Limits are configurable policy.
if !loginLimiter.Allow("ip:"+peerIP(r.RemoteAddr), time.Now()) {
    w.Header().Set("Retry-After", "60")
    http.Error(w, "too many attempts", http.StatusTooManyRequests)
    return
}
// Under a.mu immediately before publishing a new Session:
active := 0
for _, existing := range a.sessions {
    if strings.EqualFold(existing.User, uname) && time.Now().Before(existing.Expires) { active++ }
}
if active >= maxSessionsPerAccount {
    a.mu.Unlock()
    return nil, fmt.Errorf("session limit reached; revoke an existing session")
}
```

**Required regression verification.** Test rate, burst, expiration, bounded key-table size, simultaneous admission, NAT/shared-IP behavior, and that refusing a new session leaves every existing valid session intact.

<a id="r05"></a>
### R05. Audit entries can amplify memory and serialize authentication on disk I/O

**Priority:** Medium · **Evidence:** Confirmed design weakness  
**Source:** [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** Auth.Append; auditKept; command audit call sites  
**Relationship to prior work:** Partially related to A11 and state-store work.

**Problem and impact.** Audit retention is bounded by entry count, not bytes. Several command/detail sources can be large. Every append marshals and rewrites the entire retained audit array while holding the same mutex used by authentication and sessions. Large audit entries or a slow filesystem therefore stall otherwise unrelated authenticated requests. Persistence failures are logged, but the API has no durable audit-health indication. The claimed login-name truncation only covers one input source.

**Suggested fix.** Enforce UTF-8-safe byte limits and secret redaction at the central audit sink, not individual callers. Give the audit store its own synchronization and bounded/rotated append storage. Preserve ordered writes and expose persistence failures; do not fix latency by silently dropping security records.

**Implementation excerpt:**

```go
func auditText(s string, max int) string {
    s = strings.ToValidUTF8(s, "\uFFFD")
    if len(s) <= max { return s }
    s = s[:max]
    for !utf8.ValidString(s) { s = s[:len(s)-1] }
    return s + " [truncated]"
}
// In the central Append path, before storing/marshalling:
e.Actor = auditText(e.Actor, 128)
e.Action = auditText(e.Action, 128)
e.Target = auditText(e.Target, 256)
e.Detail = auditText(e.Detail, 4096)
// Move audit slice/write ordering to a separate auditMu/store, NOT a.mu.
// Passwords, session tokens and RCON secrets must never be admitted as detail.
```

**Required regression verification.** Append maximum-size command/audit input while repeatedly authenticating requests. Assert bounded retained bytes, correct truncation of UTF-8, no credential leakage, ordered records, and an observable disk-error state.

<a id="r06"></a>
### R06. Cookie-authenticated mutations lack explicit cross-origin protection

**Priority:** High · **Evidence:** Conditional security exposure; confirmed missing control  
**Source:** [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go); [`internal/arcade/api.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)  
**Relevant code:** HTTP middleware and JSON mutation handlers  
**Relationship to prior work:** Known-open A13; independently corroborated.

**Problem and impact.** The mutation routes accept JSON without consistently requiring its media type and lack a central Origin/CSRF check. SameSite=Lax is useful, but it is not an origin boundary: a hostile same-site sibling origin or another service on a different localhost port can be relevant. This is not a claim that an arbitrary cross-site POST always carries the session cookie. The exact exploitability depends on cookie scope, deployment, and browser request context.

**Suggested fix.** Wrap browser-facing routes in Go's `http.CrossOriginProtection`, available to the declared toolchain, and apply H01's strict JSON decoder to JSON-bearing endpoints. Configure only explicit trusted public origins; retain origin checks on WebSockets and MCP separately. Do not bypass the entire API for reverse-proxy convenience.

**Implementation excerpt:**

```go
func protectHTTP(next http.Handler, trustedOrigins []string) (http.Handler, error) {
    p := http.NewCrossOriginProtection()
    for _, origin := range trustedOrigins {
        if err := p.AddTrustedOrigin(origin); err != nil { return nil, err }
    }
    return p.Handler(next), nil
}
// At application construction:
// protected, err := protectHTTP(mux, configuredOrigins)
// handler := auth.attach(protected)
// JSON routes call DecodeJSON; bodiless lifecycle routes keep no-body handling.
```

**Required regression verification.** Browser/integration tests must cover ordinary cross-site, same-site cross-origin, allowed same-origin, missing Origin for non-browser clients, text/plain JSON, and reverse-proxy HTTPS. No state change may precede rejection.

**Primary external reference:** [Go CrossOriginProtection](https://pkg.go.dev/net/http#CrossOriginProtection).

<a id="r07"></a>
### R07. Secure session cookies depend on backend TLS rather than the public deployment

**Priority:** Medium · **Evidence:** Confirmed deployment-dependent weakness  
**Source:** [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go); [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go)  
**Relevant code:** session-cookie creation/deletion; HTTP-only Run listener  
**Relationship to prior work:** Known-open A13 deployment half.

**Problem and impact.** Secure is derived from `r.TLS`. With HTTPS terminated at a reverse proxy and HTTP between proxy and panel, that value is nil even though the public deployment should require secure cookies. Blindly trusting X-Forwarded-Proto would introduce a different trust problem. Login/setup/logout must use one consistent cookie policy.

**Suggested fix.** Add validated public-origin/secure-cookie configuration to the application. Require HTTPS for a non-loopback production origin. Do not switch to a `__Host-` name without migrating every reader/deleter and expiring the old cookie. The following keeps the existing cookie name for a localized change.

**Implementation excerpt:**

```go
type CookiePolicy struct { Secure bool }
func (p CookiePolicy) session(token string, expires time.Time) *http.Cookie {
    return &http.Cookie{
        Name: "gss_session", Value: token, Path: "/", HttpOnly: true,
        Secure: p.Secure, SameSite: http.SameSiteLaxMode, Expires: expires,
    }
}
func (p CookiePolicy) clear() *http.Cookie {
    c := p.session("", time.Unix(1, 0))
    c.MaxAge = -1
    return c
}
// Resolve CookiePolicy once from a validated public URL / explicit local-dev
// mode. Do not let arbitrary request headers choose it.
```

**Required regression verification.** Assert identical path/domain/Secure attributes for setup, login, reset-related refresh and logout behind a simulated TLS-terminating proxy. Verify plain-HTTP production configuration is refused.

<a id="r08"></a>
### R08. MCP raw console access defeats the advertised narrow capability boundary

**Priority:** High · **Evidence:** Confirmed policy bypass; token holder required  
**Source:** [`internal/arcade/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/mcp.go); [`internal/mcp/tools.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/tools.go)  
**Relevant code:** console command filtering; MCP tool exposure  
**Relationship to prior work:** Known-open A27; independently corroborated.

**Problem and impact.** The console denylist examines a command's leading verb, including a namespace-stripping improvement. Nested commands and server/plugin aliases are not constrained by that first verb. For example, a game command wrapper can perform a forbidden permission operation without its leading verb being `op`. Thus a token described as narrower than full panel access can still perform powerful game-administration actions. The token is required; this is not unauthenticated execution.

**Suggested fix.** Stop exposing arbitrary console text as the restricted tool. Define typed tools with explicit per-server scopes and fixed transport commands. If raw console remains, name and document it as a powerful separately granted capability. H06 contains a fixed-command dispatcher with an injected authorization check and no arbitrary fallback.

**Implementation excerpt:**

```go
type AgentAction string
const (
    AgentRead AgentAction = "server.read"
    AgentStart AgentAction = "server.start"
    AgentStop AgentAction = "server.stop"
    AgentRestart AgentAction = "server.restart"
    AgentBackup AgentAction = "server.backup"
)
// Token records gain server IDs + explicit actions; deny by default.
func permits(actions []AgentAction, wanted AgentAction) bool {
    for _, a := range actions { if a == wanted { return true } }
    return false
}
// Remove raw console send from the default tool registry. A namespaced or
// wrapped raw command must not be interpreted as a substitute for these tools.
```

**Required regression verification.** Test nested/namespaced/aliased commands, unknown tools, cross-server IDs, revoked tokens, and empty scopes. Confirm existing broad tokens require an explicit migration decision rather than silently retaining the old authority.

<a id="r09"></a>
### R09. The MCP HTTP/JSON-RPC layer accepts malformed or ambiguous requests

**Priority:** Medium · **Evidence:** Confirmed protocol and validation gaps  
**Source:** [`internal/mcp/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/mcp.go); [`internal/arcade/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/mcp.go)  
**Relevant code:** HTTP transport; request decoding; bearer extraction; dispatch  
**Relationship to prior work:** Related to deferred transport/capability hardening.

**Problem and impact.** The custom transport does not consistently validate JSON-RPC version/ID shape, distinguish notifications from requests, validate a supplied Origin, or require a well-formed Bearer scheme. A limited reader alone does not prove the entire body fits the limit: a valid JSON prefix can be decoded without validating the remainder. Authentication errors are mixed into JSON-RPC responses rather than consistently represented at the HTTP authentication boundary. These are interoperability and defense-in-depth problems; lack of a GET stream alone is not a finding.

**Suggested fix.** Apply strict bounded decoding and explicit bearer parsing before dispatch, reject invalid origins according to the MCP transport specification, and implement notification response semantics deliberately. Negotiate supported protocol versions instead of blindly claiming every version. Use an HTTP 401 challenge for missing/invalid bearer credentials. MCP specifically forbids null request IDs and requires string/integer IDs; generic JSON-RPC permits null. The sample intentionally implements the stricter MCP contract and a signed-64-bit numeric-ID policy; document that numeric range or use an arbitrary-precision integer validator.

**Implementation excerpt:**

```go
func bearerToken(h string) (string, error) {
    parts := strings.Fields(h)
    if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
        return "", fmt.Errorf("Bearer authentication required")
    }
    return parts[1], nil
}
func validMCPID(raw json.RawMessage) bool {
    if len(raw) == 0 { return true } // Notification: never send a JSON-RPC response.
    var v any
    d := json.NewDecoder(bytes.NewReader(raw)); d.UseNumber()
    if d.Decode(&v) != nil { return false }
    switch value := v.(type) {
    case string: return true
    case json.Number:
        // MCP requests use integer IDs. Keep the original RawMessage for echo.
        _, err := value.Int64()
        return err == nil
    default: return false // MCP forbids null, unlike generic JSON-RPC.
    }
}
// Require jsonrpc == "2.0". Parse errors / invalid requests get their standard
// JSON-RPC codes; valid notifications never get a result/error body.
```

**Required regression verification.** Cover missing IDs, null/bool/object IDs, invalid versions, two concatenated objects, over-limit trailing data, missing/wrong Bearer scheme, invalid Origin, cancellation and protocol negotiation.

**Primary external reference:** [MCP base messages](https://modelcontextprotocol.io/specification/2025-06-18/basic), [MCP HTTP transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports), and [JSON-RPC 2.0](https://www.jsonrpc.org/specification).

<a id="r10"></a>
### R10. Long-running HTTP work can outlive a failed response without a stable operation identity

**Priority:** High · **Evidence:** Confirmed timeout mismatch; isolated runtime proof  
**Source:** [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go); [`internal/arcade/plugins.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/plugins.go); [`internal/mcp/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/mcp.go)  
**Relevant code:** WriteTimeout; backup/restore/install/task handlers  
**Relationship to prior work:** Known-open A14/A38 async-operation work; additional timeout consequence.

**Problem and impact.** The server-wide write timeout and per-operation work timeouts are not aligned. In particular a plugin request can have a longer operation context than the HTTP write deadline. A write deadline expiring does not itself cancel handler work; local proof 09 demonstrates that distinction. Some backup paths remove deadlines entirely after parsing. Clients can consequently see failure, retry, and receive ambiguous or duplicated effects while an operation still runs. Request-context checks help, but do not create a durable job/result protocol.

**Suggested fix.** Use 202 + durable operation IDs for long tasks, bind idempotency keys to principal/server/action/body digest, and expose progress/outcome. H07 supplies a bounded worker queue; persistence of the operation and its deduplication record must occur before enqueue/202. Until migrated, impose an explicit operation deadline shorter than the response budget and recheck it before publication.

**Implementation excerpt:**

```go
// Temporary synchronous containment for an operation expected to fit:
ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
defer cancel()
// Thread ctx through download/copy/command work, not only the first request.
// Immediately before any irreversible publication:
if err := ctx.Err(); err != nil { return err }
// Async API response shape after a durable operation record is committed:
type AcceptedOperation struct {
    ID string `json:"id"`
    State string `json:"state"`
    StatusPath string `json:"status_path"`
}
// HTTP 202; Location: /api/operations/<id>. A timeout must not be called a
// rollback unless the transaction actually rolled back.
```

**Required regression verification.** Set tiny HTTP and work deadlines independently; assert no late synchronous publication. For async jobs, disconnect/retry/restart the panel and verify one identified operation, preserved result, and explicit unknown/recovery states after interrupted effects.

**Primary external reference:** [Go HTTP server deadlines](https://pkg.go.dev/net/http#Server).

<a id="r11"></a>
### R11. WebSocket connections and dead-room joins lack complete admission control

**Priority:** Medium · **Evidence:** Confirmed resource/lifecycle gap  
**Source:** [`internal/arcade/api.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api.go); [`internal/arcade/hub.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hub.go)  
**Relevant code:** console WebSocket handler; Hub.Join; DropRoom  
**Relationship to prior work:** Additional admission hardening around previously fixed hub behavior.

**Problem and impact.** SSE has a global admission limit, but console WebSocket sessions do not have equivalent global/per-principal limits. A viewer can hold many upgraded connections and associated buffers/goroutines. A dead-room/tombstone rejection also needs a clear return contract to its HTTP caller; silently declining a join while leaving an upgraded connection alive is not useful cleanup. Existing room-lock/backpressure fixes should be retained.

**Suggested fix.** Acquire a connection lease before upgrading, release on every exit, and make joining a room return an error. Revalidate server registration in coordination with deletion, then close a rejected upgrade promptly. Add per-account quotas in addition to the global bound.

**Implementation excerpt:**

```go
type ConnectionLimit chan struct{}
func (l ConnectionLimit) Acquire() (func(), bool) {
    select {
    case l <- struct{}{}:
        var once sync.Once
        return func() { once.Do(func() { <-l }) }, true
    default:
        return nil, false
    }
}
// Before websocket.Accept:
release, ok := wsLimit.Acquire()
if !ok { http.Error(w, "connection limit reached", http.StatusTooManyRequests); return }
defer release()
// Proposed Hub.Join(... ) error: return ErrRoomDeleted rather than a silent
// no-op; its caller must close the accepted connection on that error.
```

**Required regression verification.** Open more than the cap, disconnect abruptly, race join against deletion, and reconnect repeatedly. Assert quotas recover and there are no orphan subscribers or upgraded idle sockets.

<a id="r12"></a>
### R12. Mutation decoding accepts trailing JSON and silently ignores typo fields

**Priority:** Medium · **Evidence:** Confirmed API validation inconsistency  
**Source:** [`internal/arcade/api.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go); [`internal/mcp/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/mcp.go)  
**Relevant code:** JSON decoding and error mapping across handlers  
**Relationship to prior work:** Cross-cutting consistency improvement.

**Problem and impact.** Many handlers decode one object without checking EOF or unknown fields. A misspelled setting can appear to succeed while doing nothing; a second JSON value or excessive trailing content can escape intended shape checks. Validation/error statuses also vary, making it hard for clients to distinguish a conflict, missing object, invalid input, and transient failure. Not all action endpoints need a JSON body, so a blanket body requirement would break valid requests.

**Suggested fix.** Use H01 `DecodeJSON` on endpoints whose contract includes JSON. Validate the resulting typed request and map errors through a small common error type. Migrate clients that currently send full stale task objects before enabling unknown-field rejection there.

**Implementation excerpt:**

```go
type apiError struct { Status int; Code, Message string }
func replyAPIError(w http.ResponseWriter, e apiError) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(e.Status)
    _ = json.NewEncoder(w).Encode(map[string]string{
        "code": e.Code, "error": e.Message,
    })
}
// Example handler body:
var in struct { Name string `json:"name"` }
if err := DecodeJSON(w, r, &in, 1<<20); err != nil {
    var input *HTTPInputError
    if errors.As(err, &input) { replyAPIError(w, apiError{input.Status, "invalid_request", input.Message}); return }
    replyAPIError(w, apiError{500, "internal_error", "request processing failed"}); return
}
```

**Required regression verification.** Table-test trailing values/garbage, unknown fields, null/empty required values, over-limit requests, and stable 400/404/409/412/413/415/429 mappings. Keep bodiless start/stop/delete tests.

## Filesystem, persistence, backup and import

<a id="r13"></a>
### R13. Recursive ownership repair follows symlinks out of the game tree

**Priority:** High · **Evidence:** Confirmed; privileged deployment required  
**Source:** [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)  
**Relevant code:** chownTree; restore ownership repair  
**Relationship to prior work:** Newly identified alternate path; main rooted file-writer fixes do not cover it.

**Problem and impact.** `chownTree` walks entries and calls path-based `os.Chown`. On a symbolic link, Chown affects the target. A root-running panel can therefore change ownership of an external target named by a game-tree symlink. Restore aggravates the surface by repairing a tree that can still contain the held pre-restore contents. A prior Lstat is not a sufficient fix because entries can change between the check and the operation.

**Suggested fix.** Repair only the extracted replacement tree before publication, not the held originals. Use a held `os.Root` and no-follow ownership operations, propagate failures, and keep the game stopped. H02 provides bounded descriptor-based traversal. Hard-linked files are a separate inode-sharing trust issue: do not claim os.Root alone isolates their ownership.

**Implementation excerpt:**

```go
// Proposed replacement operation for each rooted entry from H02:
func repairEntryOwner(root *os.Root, name string, uid, gid int) error {
    // Root confines ancestors; Lchown never follows the final symlink.
    if err := root.Lchown(name, uid, gid); err != nil {
        return fmt.Errorf("repair ownership of %q: %w", name, err)
    }
    return nil
}
// Restore ordering changes:
// extract into staging/new -> validate -> repair ONLY staging/new -> durable
// swap. Never recursively chown staging/old or an untrusted absolute path.
// Prefer descriptor Chown for opened regular replacement files when writing.
```

**Required regression verification.** Create a symlink inside a test tree to an external sentinel and verify the sentinel's UID/GID never changes. Race symlink replacement against traversal. Inject chown failure and require a failed, recoverable restore rather than success.

**Primary external reference:** [Go Root.Lchown](https://pkg.go.dev/os#Root.Lchown).

<a id="r14"></a>
### R14. Import and ancillary readers still use check-then-open paths outside os.Root

**Priority:** High · **Evidence:** Confirmed race/confinement gap; mutable source tree required  
**Source:** [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** copyTree/readSmall; player-list readers; dynamic plugin configuration reads  
**Relationship to prior work:** Alternate-path gap beyond earlier main file API fixes.

**Problem and impact.** The main file API has substantially stronger rooted I/O, but import copying and several ancillary readers still stat/walk a pathname and then read/open it by its original absolute name. A game/plugin or another manager can change that entry between the check and the open. This can substitute an external symlink target or a FIFO, bypass an intended size check, or cause unbounded work. An import being admin-only does not make a live source directory immutable.

**Suggested fix.** Use one held root per source and descriptor metadata from the opened file, with nonblocking regular-file checks and byte limits. Apply the same reader to player lists and Geyser configuration. Treat source consistency and permissions separately from lexical path validation.

**Implementation excerpt:**

```go
// Go 1.26 / Linux-Darwin reference helper is in H02.
// Replace stat-then-os.ReadFile with:
f, info, err := openRegularRooted(sourceRoot, relativeName)
if err != nil { return err }
defer f.Close()
if info.Size() > maxAllowed { return fmt.Errorf("file too large") }
b, err := io.ReadAll(io.LimitReader(f, maxAllowed+1))
if err != nil { return err }
if int64(len(b)) > maxAllowed { return fmt.Errorf("file grew beyond limit") }
// Copy only the descriptor's admitted length; never follow an absolute path
// captured by WalkDir after a separate type check.
```

**Required regression verification.** Run symlink-swap/FIFO/size-growth tests on imports, player-list reads and dynamic port reads, not only `/file`. Require a bounded error and verify no external sentinel bytes are incorporated into output.

<a id="r15"></a>
### R15. Directory opens can block on a FIFO before checking that it is a directory

**Priority:** Medium · **Evidence:** Confirmed; isolated FIFO proof  
**Source:** [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go); [`internal/arcade/plugins.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/plugins.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)  
**Relevant code:** ListFiles; plugin directory listing; recursive archive directory traversal  
**Relationship to prior work:** Incomplete special-file handling around A08/A17.

**Problem and impact.** Several paths open an alleged directory with an ordinary read open and only determine its type afterwards. Pointing the file-list route at a FIFO can block before any directory check. Directory replacement during archiving can produce the same class of problem and prolong the save-off window. Existing nonblocking regular-file helpers do not automatically protect directory opens. Proof 05 confirms the blocking-open primitive.

**Suggested fix.** Use O_DIRECTORY together with nonblocking/no-follow final-component flags for directory descriptors. Iterate bounded batches. Use the opened descriptor throughout instead of reopening its pathname after validation.

**Implementation excerpt:**

```go
func openDirectoryRooted(root *os.Root, name string) (*os.File, error) {
    f, err := root.OpenFile(name,
        os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
    if err != nil { return nil, err }
    st, err := f.Stat()
    if err != nil || !st.IsDir() {
        _ = f.Close()
        if err != nil { return nil, err }
        return nil, fmt.Errorf("not a directory")
    }
    return f, nil
}
```

**Required regression verification.** A viewer listing a FIFO must receive a bounded error without a writer opening the pipe. Swap a directory for a FIFO during archive traversal and assert save resumption/operation cleanup. Test directory symlink policy explicitly.

<a id="r16"></a>
### R16. Registration checks and filesystem locks are not applied uniformly

**Priority:** High · **Evidence:** Confirmed lifecycle consistency gap  
**Source:** [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go)  
**Relevant code:** requireRegistered call sites; ensureServerDir; restore/settings/resources/player-list entry points  
**Relationship to prior work:** Incomplete A22 coverage; related to A15 transaction serialization.

**Problem and impact.** Some entry points check registration before waiting for a filesystem gate, or omit the post-gate check. A queued operation can retain an old Server pointer after deletion and later mutate/recreate its directory or save stale state. Read paths that call a directory-creation helper make this worse. The pass-6 registration safeguard is useful, but it must be a common invariant rather than a selected-call-site convention. Shared fs locks also do not serialize read-modify-write edits with each other.

**Suggested fix.** For each operation, acquire its fs gate first and then check pointer identity under the manager lock. A read must never call mkdir. Serialize all panel read-modify-write edits, including deletes and player-list edits, with the edit gate. Preserve the existing fs-before-lifecycle ordering; never call Save with s.mu held.

**Implementation excerpt:**

```go
func (m *Manager) registeredIdentity(s *Server) bool {
    m.mu.RLock()
    defer m.mu.RUnlock()
    // Compare pointer identity, not merely membership by a possibly reused ID.
    return m.servers[s.ID] == s
}
// At a mutation's entry, BEFORE any ensure/create/write:
s.fsMu.RLock()
defer s.fsMu.RUnlock()
if !m.registeredIdentity(s) { return fmt.Errorf("server no longer registered") }
s.editMu.Lock()
defer s.editMu.Unlock()
// Exclusive restore/delete paths use fsMu.Lock, then the same identity check.
// Read paths use os.OpenRoot on an existing directory; no ensureServerDir.
```

**Required regression verification.** Barrier-test deletion winning while a read/edit/restore is waiting. Assert no directory is recreated, no stale pointer is accepted, no registry entry is resurrected, and mixed edit paths cannot lose one another's changes.

<a id="r17"></a>
### R17. State replacement is atomic in name but not durably committed

**Priority:** High · **Evidence:** Confirmed persistence gap  
**Source:** [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go); [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go); [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** writeFileAtomic; writeAtomicIn; callers committing users/tokens/tasks/servers  
**Relationship to prior work:** Residual of A10/A18; helper test in this audit covers only the success path.

**Problem and impact.** The common state writer does not complete the file-sync and parent-directory-sync sequence. A successful response for a credential revocation, task change or server update can be lost after a crash/power failure. The rooted file writer has stronger sync behavior, but a directory-sync failure after rename is a different outcome from a pre-publication failure. Treating both as a generic error and reverting memory can leave disk and memory disagreeing about an already visible commit.

**Suggested fix.** Use H01 `AtomicStateWrite` for private state files and propagate a `CommitOutcome{Published, Durable}` through callers. Preserve the rooted writer for game trees. If publication already happened, publish the matching in-memory state/revocations and report degraded durability; do not pretend the old state is still authoritative. This does not make multiple separate files transactional.

**Implementation excerpt:**

```go
outcome, err := AtomicStateWrite(statePath, encodedNext, 0o600)
if outcome.Published {
    // Publish matching model state even when directory fsync returned an error.
    // For credential deletion/reset, revoke affected sessions in this branch.
    publishNextState()
}
if err != nil {
    return fmt.Errorf("state update published=%t durable=%t: %w",
        outcome.Published, outcome.Durable, err)
}
// publishNextState is the caller's in-memory commit closure, invoked while
// its store transaction lock is held. It must not perform another disk write.
```

**Required regression verification.** Fault-inject write, file Sync, Close, Rename and directory Sync independently. Check old/new bytes and model state at each boundary. Separately run crash/power-loss tests on supported filesystems; ordinary helper unit tests do not prove power-loss durability.

<a id="r18"></a>
### R18. Boot recovery can discard staging when reading held originals fails

**Priority:** High · **Evidence:** Confirmed dangerous error branch  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** recoverInterruptedRestores  
**Relationship to prior work:** Incomplete A02/A03 crash-recovery handling.

**Problem and impact.** Recovery groups an error from reading `old/` with the empty-directory case and attempts to remove the staging tree. An I/O error is not evidence that there are no originals. A transient read failure followed by successful removal can destroy the only retained recovery copy. With some permission failures RemoveAll will also fail, so destruction is conditional rather than inevitable. The code must not rely on that accidental protection.

**Suggested fix.** Differentiate absent, empty and unreadable states. Preserve all recovery material and block startup/adoption of the affected server on unknown state. Without a durable journal, even a missing `old/` should not automatically establish that arbitrary staging is safe to delete. Change recoverInterruptedRestores to return an error and make Load check it before recoverAfterBoot; a global fail-closed startup error is a safe immediate containment alternative to per-server recovery mode.

**Implementation excerpt:**

```go
entries, err := os.ReadDir(held)
if err != nil {
    // Do not RemoveAll(staging) in this branch, including on an unexpected
    // ENOENT unless a trusted journal proves extraction never advanced.
    return fmt.Errorf("recovery blocked; retained %s: %w", staging, err)
}
if len(entries) == 0 {
    // Cleanup is permitted only after validating the journal phase and
    // confirming there are no original entries awaiting restoration.
    return fmt.Errorf("empty recovery holding area requires journal validation: %s", staging)
}
// Continue recovery; every remove/rename failure preserves the journal and
// remaining originals and keeps this server blocked.
```

**Required regression verification.** Inject EIO and EACCES into reading held entries, then allow RemoveAll to succeed. Assert RemoveAll is never invoked, originals remain intact and no auto-start occurs. Existing successful rollback fixtures do not cover this branch.

<a id="r19"></a>
### R19. Restore lacks a durable transaction connecting the tree, metadata and commit marker

**Priority:** High · **Evidence:** Confirmed crash-consistency gap  
**Source:** [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** RestoreBackup; .committed; reloadProps; boot recovery  
**Relationship to prior work:** Known-open multi-resource transactions, plus incomplete earlier restore claims.

**Problem and impact.** The restore uses staging and rollback, but the rename sequence, metadata save and plain commit-marker write are not one durable transaction. The tree can be committed differently from the persisted port/settings after a crash. Marker/ownership/reload failures are not all treated as failed transactions. A crash during publication can also leave new-only entries that cannot be classified using only the set of names found in held originals. The historical fixes addressed different partial states; none substitutes for an explicit manifest and write ordering.

**Suggested fix.** Record a complete original/replacement entry inventory and old/new metadata in a private durable journal before evacuation. Sync files and affected directories before advancing phases. Recover before adopting containers. H04 provides manifest validation, durable phase publication and an idempotent rollback implementation; integrating it requires the corresponding ordered rename path and crash-injection tests, not just adding a marker.

**Implementation excerpt:**

```go
type RestorePhase string
const (
    RestorePrepared RestorePhase = "prepared"
    RestoreInstalling RestorePhase = "installing"
    RestoreCommitted RestorePhase = "committed"
)
type RestoreJournal struct {
    Version int `json:"version"`
    ServerID string `json:"server_id"`
    Phase RestorePhase `json:"phase"`
    OriginalNames []string `json:"original_names"`
    ReplacementNames []string `json:"replacement_names"`
    OldModel json.RawMessage `json:"old_model"`
    NewModel json.RawMessage `json:"new_model"`
}
// Commit order: durable prepared journal -> evacuate + sync -> durable
// installing phase -> install + sync -> publish new metadata + sync ->
// durable committed phase -> cleanup. Any uncertainty keeps recovery blocked.
```

**Required regression verification.** Kill a subprocess after every journal write, rename and sync boundary, then restart recovery twice. Require either the complete old generation or complete new generation and matching port/model state, never a mixed tree. Include new-only entries and failure during rollback itself.

<a id="r20"></a>
### R20. An incomplete recovery is retained on disk but not represented as a blocked server state

**Priority:** High · **Evidence:** Confirmed safety-state omission  
**Source:** [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** rollback failure handling; Start; reconcile/auto-resume  
**Relationship to prior work:** New interlock requirement for known recovery failure paths.

**Problem and impact.** Retaining staging after a rollback error is necessary, but the server can still look stopped/failed rather than recovery-blocked. Manual or automatic start can then run a partially restored tree. A log line naming staging is not an enforceable interlock, and a process restart loses an in-memory warning unless the block is persisted outside the mutable tree.

**Suggested fix.** Add a durable RecoveryRequired state/operation ID. Apply it to start, restore, destructive file edits, clone and scheduled actions. Only a successful verified recovery or an explicit audited administrative resolution may clear it. A journal discovered at boot is enough to keep the interlock active until resolved.

**Implementation excerpt:**

```go
// Proposed persisted field on the server record:
// RecoveryOperation string `json:"recovery_operation,omitempty"`
func recoveryGuard(operation string) error {
    if operation != "" {
        return fmt.Errorf("server is blocked pending recovery operation %s", operation)
    }
    return nil
}
// In claimStart and every mutable-operation admission, under s.mu:
if err := recoveryGuard(s.RecoveryOperation); err != nil { return err }
// On uncertain/incomplete rollback, durably set the field BEFORE allowing
// lifecycle admission again. Boot loads journals before starting workers.
```

**Required regression verification.** Force rollback failure, invoke manual Start, scheduler Start, auto-resume, clone and file mutations, then restart the panel. All must refuse until the recovery operation is explicitly completed.

<a id="r21"></a>
### R21. Deletion destroys data before the registry removal is durable

**Priority:** High · **Evidence:** Confirmed irreversible transaction ordering  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** Manager.Delete; directory/backup cleanup; Save  
**Relationship to prior work:** Known-open A21; error propagation alone did not close transaction risk.

**Problem and impact.** Deletion removes in-memory registration and performs destructive filesystem cleanup before the final registry save is safely committed. Surfacing cleanup/Save errors is better than hiding them, but does not restore destroyed worlds or backups. If persistence fails, an old on-disk registry can reappear on restart pointing at deleted data. Adopted-directory behavior must remain distinct from deletion of panel-owned data and backups.

**Suggested fix.** Commit a durable tombstone/operation before destructive work. Move panel-owned trees to a same-filesystem trash location, atomically publish registry removal, then garbage-collect according to policy. Expose separate detach/delete-managed-data/delete-backups intent. H04's journal primitives apply, but deletion needs its own manifest and retention policy.

**Implementation excerpt:**

```go
type DeleteIntent struct {
    ServerID string `json:"server_id"`
    OperationID string `json:"operation_id"`
    DeleteManagedData bool `json:"delete_managed_data"`
    DeleteBackups bool `json:"delete_backups"`
    Adopted bool `json:"adopted"`
}
func (d DeleteIntent) Validate() error {
    if d.ServerID == "" || d.OperationID == "" { return fmt.Errorf("missing deletion identity") }
    if d.Adopted && d.DeleteManagedData { return fmt.Errorf("adopted directories may only be detached") }
    return nil
}
// Persist validated intent -> rename owned data to trash + sync parents ->
// durably remove registry entry -> mark operation committed -> later GC.
// Never remove an adopted external directory as rollback/cleanup.
```

**Required regression verification.** Inject registry-save and trash-rename failures; crash between every phase. Repeated recovery must not resurrect an apparently healthy empty server or delete an adopted source. Verify backup-deletion intent separately.

<a id="r22"></a>
### R22. Startup cleanup still recognizes an overly broad temporary-file suffix

**Priority:** Medium · **Evidence:** Confirmed cleanup policy risk  
**Source:** [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go)  
**Relevant code:** sweepTempFiles  
**Relationship to prior work:** Residual cleanup ownership problem after earlier prefix fixes.

**Problem and impact.** Although panel-prefixed temporary names improved cleanup, a suffix-based `.tar.gz.part` rule still treats otherwise user-named files as disposable. A legitimate partial upload/download with that suffix can be removed during startup. Cleanup must identify ownership, not infer it from a common extension, especially when walking directories shared with game software.

**Suggested fix.** Limit cleanup to panel-owned temporary namespaces and completed operation manifests. Stop applying generic suffix rules to user trees. Unknown leftovers should be reported or retained for operator review, not deleted speculatively.

**Implementation excerpt:**

```go
func panelOwnedTempName(name string) bool {
    return strings.HasPrefix(name, ".arcade-tmp-state-") ||
        strings.HasPrefix(name, ".arcade-tmp-backup-")
}
// Apply only within the corresponding private state/backup temp directories,
// not recursively to arbitrary game files. Legacy .part files require a
// migration inventory proving that the panel created them.
// Remove the generic strings.HasSuffix(name, ".tar.gz.part") deletion rule.
```

**Required regression verification.** Seed both a genuine panel temporary and `user-export.tar.gz.part`; restart and assert only the owned temporary is eligible for cleanup. Repeat for adopted paths and unknown interrupted restore directories.

<a id="r23"></a>
### R23. Settings and resource mutations publish partial state on persistence failure

**Priority:** High · **Evidence:** Confirmed multi-resource consistency defect  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)  
**Relevant code:** ApplySettings; SetResources; Reorder; retention updates  
**Relationship to prior work:** Known-open A10 multi-resource state; independently corroborated.

**Problem and impact.** Several mutations change the live model before persistence is known to have succeeded. ApplySettings also spans the model, registry and server.properties, with failures capable of leaving only some updated. A returned error does not imply no change occurred. Resource/reorder/retention paths have simpler versions of the same publish-before-commit problem. Port reservation changes add another participant to the settings transaction.

**Suggested fix.** Build a deep-copied candidate, validate and reserve against the whole candidate, persist through a transaction, and only then publish it. For one-file state, use H01's publication-aware outcome; for settings plus properties use H04's durable journal and recovery guard. Distinguish desired settings from the last successfully applied launch settings.

**Implementation excerpt:**

```go
type ConfigCandidate struct {
    Props map[string]string
    Pending []string
    Port, MemoryMB int
    CPU float64
}
func copyProps(in map[string]string) map[string]string {
    out := make(map[string]string, len(in))
    for k, v := range in { out[k] = v }
    return out
}
// Under edit admission: snapshot -> build ConfigCandidate -> validate all
// keys/port geometry/host limits -> acquire proposed binding lease -> persist
// model + properties via the journal -> publish candidate -> release old lease.
// On pre-publication failure, release only the new lease and preserve old state.
// On uncertain publication, block rather than blindly reverting one participant.
```

**Required regression verification.** For every mutation, fault-inject state write, properties write and lease publication. Assert the returned commit state matches disk/model/port ledger. Test a later unrelated Save cannot silently persist an earlier rejected mutation.

<a id="r24"></a>
### R24. Port changes can lose restart warnings, and later edits erase earlier warnings

**Priority:** Medium · **Evidence:** Confirmed; isolated control-flow proof  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** ApplySettings; changeServerPort; PendingRestart  
**Relationship to prior work:** Incomplete pending-state behavior despite earlier port-ledger fixes.

**Problem and impact.** ApplySettings commits the port change before comparing the properties values used to build restart requirements. That comparison can therefore observe the new port as already unchanged. It then replaces PendingRestart with the keys from the latest edit, discarding older pending resource/settings changes; even an unrelated/no-op edit can clear a real warning. Proof 08 reproduces both mechanisms.

**Suggested fix.** Capture the pre-edit values before changing the port. Merge newly pending keys with existing pending state as an immediate patch. The stronger model derives pending keys by comparing desired configuration with the immutable last successful launch configuration, and clears only the generation actually applied.

**Implementation excerpt:**

```go
// Under the edit transaction, BEFORE changeServerPort:
s.mu.Lock()
oldProps := copyProps(s.Props)
priorPending := append([]string(nil), s.PendingRestart...)
s.mu.Unlock()
// Compute differences against oldProps, not the already changed live map.
// After the transaction's candidate is committed:
s.mu.Lock()
s.PendingRestart = MergePending(priorPending, changedRestartKeys)
s.mu.Unlock()
// MergePending is implemented and tested in H01. A no-op edit must not clear
// previously pending keys; a completed matching-generation restart may clear.
```

**Required regression verification.** Change a live port, then memory, then an unrelated property and finally submit a no-op. Every unapplied change must remain visible. Test edits made while a restart is starting; they must not be cleared by the earlier launch.

<a id="r25"></a>
### R25. Managed properties are parsed and emitted differently from Java Properties

**Priority:** High · **Evidence:** Confirmed parsing/serialization mismatch; Java reference checks  
**Source:** [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go)  
**Relevant code:** propsPortValue; hasPortLine; reloadProps; writeProps; ApplySettings  
**Relationship to prior work:** Incomplete A16 parser unification and input validation.

**Problem and impact.** The local parser does not implement Java Properties separators/escapes/continuations, and managed-port readers are not all equivalent. In particular, skipping invalid numeric occurrences can retain an earlier valid port even though the game's final value is invalid. A colon separator or continued value can also disagree with the panel's interpretation. Raw newline-containing settings written as `key=value` can inject additional lines. Java reference cases in H09 demonstrate colon, last-value and continuation semantics. Typed settings additionally need enum/bool/integer range checks, not just string assignment.

**Suggested fix.** Use one specification-compatible, bounded parser/serializer everywhere the managed identity is read or written. As a conservative immediate alternative, explicitly reject unsupported syntax, duplicate managed keys, control characters and invalid managed values rather than silently guessing. H03 implements this restricted safety mode and documents its compatibility cost. Apply it consistently to raw edits, imports and restored properties.

**Implementation excerpt:**

```go
func validatePropertyValue(key, value string) error {
    if strings.ContainsAny(value, "\x00\r\n") {
        return fmt.Errorf("%s must not contain NUL or line breaks", key)
    }
    if strings.Contains(value, `\`) {
        return fmt.Errorf("escaped values require the full properties serializer")
    }
    if key == "server-port" {
        p, err := strconv.Atoi(value)
        if err != nil || p < 1 || p > 65535 { return fmt.Errorf("invalid server-port") }
    }
    return nil
}
// H03 parses a deliberately restricted key=value format and refuses ambiguous
// syntax. Do not keep the current 'invalid value means reuse old port' rule.
// After parsing, validate full template binding geometry before publication.
```

**Required regression verification.** Differential-test against Java Properties for supported syntax; explicitly reject every unsupported case. Include invalid final duplicates, colon/space separators, escaped keys, continuation, Unicode escapes, newline MOTD injection, overflows, missing managed identity and restore/import paths.

**Primary external reference:** [Java Properties loading semantics](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/Properties.html).

<a id="r26"></a>
### R26. Several settings advertised as immediate are only written to disk

**Priority:** High · **Evidence:** Confirmed behavior/contract mismatch  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js)  
**Relevant code:** propSchema Applies metadata; ApplySettings; settings UI  
**Relationship to prior work:** Additional product-safety mismatch; not closed by persistence fixes.

**Problem and impact.** The schema labels settings such as whitelist/max players/view or simulation distance as immediately applicable, but the settings path primarily updates the properties file. It does not implement a verified live command/reload for every such property. A whitelist control is especially consequential: a saved UI value must not imply the running game is already enforcing it. This is a mismatch between this handler and its advertised contract, not an assertion that every game has identical reload semantics.

**Suggested fix.** Make the default application mode `next_restart`. Only mark a setting immediate when its game adapter actually applies it and verifies success. Return separate desired/applied values and pending status. Do not reuse a successful disk write as evidence of runtime enforcement.

**Implementation excerpt:**

```go
type LiveSettingAdapter interface {
    ApplyAndVerify(context.Context, string, string) error
}
func settingApplyMode(hasVerifiedAdapter bool) string {
    if hasVerifiedAdapter { return "immediate" }
    return "next_restart"
}
// Update each file-only key's Applies metadata to "next_restart". Use the
// adapter contract for any future immediate setting; after disk persistence,
// only a successful ApplyAndVerify may change its applied runtime value.
// A failed live apply leaves desired state pending and returns a clear result.
```

**Required regression verification.** Start an actual supported game, change each allegedly immediate setting through the API, and query the runtime—not the file—for the resulting state. Until that succeeds, assert the API/UI reports restart required.

<a id="r27"></a>
### R27. Live snapshot success is not a verified save-state contract; clone drops resume errors

**Priority:** High · **Evidence:** Confirmed error handling plus unverified game acknowledgment  
**Source:** [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** quiesceForBackup; CreateBackup; Clone; query transport  
**Relationship to prior work:** Known-open A06 acknowledgment contract plus newly identified clone resume omission.

**Problem and impact.** A successful console transport call is not necessarily proof that the game accepted save-off or completed a flush. A fixed settling delay cannot establish that invariant. Some failure cleanup attempts also ignore the save-on result. CreateBackup now surfaces its final resume failure, but clone still defers the resume function without incorporating its error, so a successful clone response can leave the source with automatic saving disabled. A published artifact and a safely resumed source are separate outcomes.

**Suggested fix.** Use typed per-game snapshot adapters with verified acknowledgments, or conservatively require the server to be stopped. Always combine resume failure with the operation result and persist an alert/interlock for an unverified save state. Do not prune older recovery copies until the entire backup/resume contract is considered successful.

**Implementation excerpt:**

```go
// In the clone worker, use a named result and replace bare `defer resume()`:
func withVerifiedSnapshot(
    begin func() (func() error, error),
    copySnapshot func() error,
) (err error) {
    resume, err := begin()
    if err != nil { return err }
    defer func() {
        if resumeErr := resume(); resumeErr != nil {
            err = errors.Join(err, fmt.Errorf("source save resumption failed: %w", resumeErr))
        }
    }()
    return copySnapshot()
}
// begin must verify game acknowledgments, not merely successful RCON delivery.
// Where that capability is absent, return a 'stop the server first' error.
```

**Required regression verification.** Fake accepted/rejected/empty/unrecognized console replies, flush timeout, copy failure and resume failure. Clone must never report an unqualified success when save-on failed. Test a real image's acknowledgment text/behavior before enabling live snapshots.

<a id="r28"></a>
### R28. Raw console commands can undermine the backup filesystem gate

**Priority:** High · **Evidence:** Confirmed coordination gap  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** Manager.Send; backup quiesce gate; scheduled console commands  
**Relationship to prior work:** Additional mutation path beyond earlier Stop/Restart/fsMu fixes.

**Problem and impact.** The backup fs gate coordinates panel file mutations and lifecycle operations, but arbitrary console commands are another way to mutate the game. External Send calls can issue save-on, stop, or world-changing commands during a quiesced backup. The scheduler uses the same raw command surface. The simulator also needs to gate the execution of queued effects, not merely their enqueue. Filesystem locking cannot constrain commands sent outside this panel, which must remain an explicit live-backup limitation.

**Suggested fix.** Gate external console mutations against snapshot operations. Use an internal transport method for the snapshot adapter's own save commands to avoid recursively taking the same lock. Prefer rejecting with a clear busy response to waiting indefinitely; require stopped snapshots for unsupported coordination models.

**Implementation excerpt:**

```go
// In external command admission (HTTP, WS, scheduler and MCP raw capability):
if !s.fsMu.TryRLock() {
    return fmt.Errorf("console mutation is temporarily blocked by a snapshot or restore")
}
defer s.fsMu.RUnlock()
if !m.registeredIdentity(s) { return fmt.Errorf("server no longer registered") }
// Dispatch the external command while the lease is held.
// Snapshot adapters call their private transport directly while holding the
// exclusive gate; they must NOT call this public gated entry point.
// Asynchronous simulator commands carry the lease until their effect finishes.
```

**Required regression verification.** Hold a backup in the quiesced phase, then issue save-on/stop/world edits from each external command path. They must fail busy or wait safely; the internal resume must still execute without deadlocking.

<a id="r29"></a>
### R29. The tar header can describe a different size than the descriptor being copied

**Priority:** Medium · **Evidence:** Confirmed; isolated tar-overflow proof  
**Source:** [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)  
**Relevant code:** tarGz regular-file header creation and io.Copy  
**Relationship to prior work:** Incomplete descriptor-metadata half of A08.

**Problem and impact.** The archive path opens regular files more safely now, but header metadata is still captured separately from the opened descriptor and the copy can run to EOF. A growing log can exceed the declared tar entry size and fail the backup; shrink/replacement can cause short copies or inconsistent metadata. Root confinement alone does not make a live directory snapshot-consistent. Proof 04 demonstrates the tar overflow when more bytes are written than the header permits.

**Suggested fix.** Take metadata from the descriptor that will be copied and copy exactly its admitted size. Treat a short read as an error. Exclude volatile logs/caches according to an explicit backup policy. Reading a fixed prefix of a growing log prevents one failure mode; it does not make a mutating world file consistent.

**Implementation excerpt:**

```go
f, info, err := openRegularRooted(root, relativeName)
if err != nil { return err }
defer f.Close()
hdr, err := tar.FileInfoHeader(info, "")
if err != nil { return err }
hdr.Name = filepath.ToSlash(relativeName)
if err := tw.WriteHeader(hdr); err != nil { return err }
n, err := io.CopyN(tw, f, info.Size())
if err != nil || n != info.Size() {
    return fmt.Errorf("snapshot file changed or read failed: %s: %w", relativeName, err)
}
// Do not copy additional bytes beyond the descriptor-derived header size.
```

**Required regression verification.** Use deterministic growing/shrinking/replaced-file fixtures. Growing excluded logs should not break a world backup; shrinking included files must fail visibly. Extract successful archives and compare headers/content to the declared snapshot policy.

**Primary external reference:** [Go archive/tar writer contract](https://pkg.go.dev/archive/tar#Writer.Write).

<a id="r30"></a>
### R30. Independent preflight free-space checks do not reserve shared storage capacity

**Priority:** High · **Evidence:** Confirmed resource-admission gap  
**Source:** [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go); [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/hostcap.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap.go)  
**Relevant code:** diskFree admission; copy/extract loops; concurrent jobs  
**Relationship to prior work:** Known-open A07; extended to all storage-heavy operations.

**Problem and impact.** Multiple imports/backups/restores can each pass a free-space check against the same filesystem and then consume the same remaining capacity. Some work estimates are based on mutable source sizes or compressed archive metadata; they are not reservations. Existing per-entry restore checks reduce damage but do not isolate concurrent operations. Ignoring a free-space query error can also turn a safety check into an implicit allow.

**Suggested fix.** Reserve capacity against filesystem identity under one manager, include staging/rollback overhead, enforce byte/file/time budgets during work, and release reservations on every terminal path. Keep headroom for live worlds. H07's bounded work admission complements but does not replace storage reservations. If a required capacity measurement fails, return unknown/refuse rather than pretending capacity exists.

**Implementation excerpt:**

```go
type DiskBudget struct {
    mu sync.Mutex
    reserved map[uint64]uint64 // key: verified filesystem identity, not path text
}
func (b *DiskBudget) Reserve(dev, free, headroom, need uint64) (func(), error) {
    b.mu.Lock()
    if b.reserved == nil { b.reserved = make(map[uint64]uint64) }
    used := b.reserved[dev]
    if free < headroom || used > free-headroom || need > free-headroom-used {
        b.mu.Unlock(); return nil, fmt.Errorf("insufficient unreserved disk capacity")
    }
    b.reserved[dev] += need
    b.mu.Unlock()
    var once sync.Once
    return func() { once.Do(func() { b.mu.Lock(); b.reserved[dev] -= need; b.mu.Unlock() }) }, nil
}
```

**Required regression verification.** Run two jobs against one nearly full filesystem and another job against a different filesystem. Verify shared reservations, overflow-safe arithmetic, failure/cancellation release, free-space query failures and live-world safety headroom.

<a id="r31"></a>
### R31. String-prefix path checks mishandle roots and sibling prefixes

**Priority:** High · **Evidence:** Confirmed lexical-boundary defects; isolated proof  
**Source:** [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go)  
**Relevant code:** import source containment; hostPathFor translation  
**Relationship to prior work:** Additional path-identity issue.

**Problem and impact.** Containment logic built from `base + separator` mishandles the filesystem root (`/` becomes `//`) and raw prefix matching can confuse sibling names such as `/data` and `/data-other`. In import, an accepted ancestor source can include the destination tree and recursively copy newly created output. In container path translation, a false prefix match can map a path to the wrong daemon-visible host location. Proof 02 reproduces the root boundary error.

**Suggested fix.** Use filepath.Rel for segment-aware lexical relationships on canonical absolute paths, and reject both source-inside-destination and source-ancestor-of-destination. This is admission logic, not a substitute for os.Root during I/O. Keep panel-visible and daemon-visible paths separate.

**Implementation excerpt:**

```go
func relativeWithin(base, target string) (string, bool) {
    rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
    if err != nil || filepath.IsAbs(rel) || rel == ".." ||
        strings.HasPrefix(rel, ".."+string(filepath.Separator)) { return "", false }
    return rel, true
}
func mapMountPath(panelMount, hostMount, panelPath string) (string, error) {
    rel, ok := relativeWithin(panelMount, panelPath)
    if !ok { return "", fmt.Errorf("path is outside the panel mount") }
    return filepath.Join(hostMount, rel), nil
}
// Import admission rejects overlap in EITHER direction after canonicalizing
// existing source and destination-parent paths. Explicitly cover base == "/".
```

**Required regression verification.** Test `/`, exact equality, trailing separators, `/data-other`, ancestors, descendants, symlinked source roots and a destination created after scanning. Assert no recursive destination copying and correct daemon mount mapping.

<a id="r32"></a>
### R32. Import does not enforce all creation invariants or freeze the operator’s selected source

**Priority:** Medium · **Evidence:** Confirmed validation gaps; source consistency is conditional  
**Source:** [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** Import request validation; scan-to-copy/adopt; finishImport  
**Relationship to prior work:** Import parity/consistency improvement beyond A42 Create-only validation.

**Problem and impact.** Import does not consistently mirror Create's runtime/effective-resource validation; invalid runtime input can fall back to simulation. A scan is also only an observation: selected files, jar candidates, source ownership and source activity can change before adoption/copy. Different imports can target the same canonical adopted directory unless path identity is reserved. Copying a server managed elsewhere while it is running is not a consistent world snapshot merely because this panel never writes the original.

**Suggested fix.** Reuse one validated creation specification for Create/Import/Clone, validate effective defaults and full binding geometry, reserve canonical adopted paths, and record an explicit jar choice plus source fingerprint. Require a stopped/snapshotted external source or an explicit unsafe-copy acknowledgment. Do not silently choose a jar/version when discovery is ambiguous.

**Implementation excerpt:**

```go
func validateRuntimeName(runtime string) error {
    switch runtime {
    case "sim", "docker": return nil
    default: return fmt.Errorf("runtime must be sim or docker")
    }
}
type ImportSelection struct {
    CanonicalSource string
    JarRelativePath string
    JarSHA256 string
    SourceConfirmedStopped bool
}
func (s ImportSelection) Validate() error {
    if !filepath.IsAbs(s.CanonicalSource) { return fmt.Errorf("canonical source required") }
    if !s.SourceConfirmedStopped { return fmt.Errorf("stop or snapshot the source before import") }
    if s.JarRelativePath != "" && !filepath.IsLocal(s.JarRelativePath) { return fmt.Errorf("invalid jar path") }
    return nil
}
// Reopen via os.Root and verify the selected descriptor's digest at commit.
```

**Required regression verification.** Test unknown runtime, over-limit effective defaults, two concurrent adoptions of one path, source mutation after scan, multiple same-family jars, and an explicitly selected jar no longer matching its fingerprint.

<a id="r33"></a>
### R33. Clone mixes mutable source state with the current template rather than a frozen launch configuration

**Priority:** Medium · **Evidence:** Confirmed snapshot/contract weakness  
**Source:** [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/templates.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/templates.go)  
**Relevant code:** clone request/worker; source metadata snapshot; template-derived fields  
**Relationship to prior work:** Related to snapshot contracts; clone resume separately tracked in R27.

**Problem and impact.** Clone captures some mutable source fields outside the complete filesystem/lifecycle snapshot window and derives launch details from the current template. After a template upgrade or server-specific customization, the cloned world can receive different image/environment/arguments/data path/console behavior. Copying the tree while using an earlier properties snapshot is also not a coherent configuration snapshot. Source defaults should not bypass current capacity admission for the destination.

**Suggested fix.** Freeze all source configuration and file state under the same snapshot transaction; deep-copy maps/slices and preserve the source's actual launch configuration unless the user explicitly requests a template migration. Validate the complete destination candidate and its port/resource leases before publication.

**Implementation excerpt:**

```go
type LaunchSnapshot struct {
    Image, DataPath, Console string
    Env map[string]string
    Args, Protocols []string
    ExtraPorts []PublishedBinding
    PortSpan, MemoryMB int
    CPU float64
    Props map[string]string
}
func copyStrings(in []string) []string { return append([]string(nil), in...) }
// While holding the source snapshot gate and s.mu, build LaunchSnapshot with
// copyProps/copyStrings. Pass that immutable value to the worker. Do not look
// up the current template again to replace source-specific launch fields.
// Destination admission validates the copied limits and full port geometry.
```

**Required regression verification.** Edit source resources/configuration while clone admission waits, then update a template between server creation and cloning. The clone must reproduce one coherent source snapshot and retain explicit overrides. Test destination host-fit rejection.

<a id="r34"></a>
### R34. Import/clone jobs need bounded ownership, durable outcomes and panic cleanup

**Priority:** Medium · **Evidence:** Confirmed operational lifecycle gap  
**Source:** [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go)  
**Relevant code:** in-memory jobs; worker launch; lease/staging cleanup  
**Relationship to prior work:** Known-open worker/async-operation work; additional cleanup requirements.

**Problem and impact.** Long jobs are chiefly in-memory objects and can be started without a single application-wide bounded work policy. A panel restart loses their status and a panic logger is not a terminal job transition. Reservations/staging/source resumption must be released on every failure, including panic and cancellation. Retention that is triggered only by a future request does not guarantee timely cleanup of finished jobs.

**Suggested fix.** Use the durable operation model from R10 plus the bounded worker pool H07. Every worker must own a defer stack for terminal-state recording, lease release, source resume and staging retention/cleanup. Persist enough information to classify interrupted jobs at boot; do not automatically replay non-idempotent effects after a crash.

**Implementation excerpt:**

```go
func runOwnedJob(ctx context.Context, work func(context.Context) error,
    finish func(error), release func()) {
    var result error
    defer release()
    defer func() {
        if p := recover(); p != nil { result = fmt.Errorf("job panicked: %v", p) }
        finish(result) // Must persist success/failure/unknown, not just log it.
    }()
    result = work(ctx)
}
// finish must mark partial publication/recovery separately from ordinary
// failure. A panic after publication is not evidence that nothing happened.
// An application-owned reaper enforces bounded completed-job retention.
```

**Required regression verification.** Inject panic at each job phase, cancel while queued/copying/publishing, fill queue capacity and restart mid-copy. Verify terminal records, no leaked port/path/disk leases, and retained recovery data for ambiguous publication.

## Runtime, resource accounting and templates

<a id="r35"></a>
### R35. Docker transport errors are treated as proof that a container is stopped

**Priority:** High · **Evidence:** Confirmed dangerous state collapse  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)  
**Relevant code:** containerRunning; watchExit; Start/remove; adoption and snapshot checks  
**Relationship to prior work:** Known-open A24 tri-state API; incomplete safety consequence of partial fix.

**Problem and impact.** `containerRunning` returns false whenever inspect fails, including daemon/transport/permission failures. Callers use that false value as absence/death in lifecycle and safety decisions. A transient daemon outage can therefore clear live reservations/supervision or allow a destructive remove/recreate path when the existing game is merely unobservable. The watch-exit retry improvement only helps when the secondary inspect positively reports running; it does not solve unknown-state handling.

**Suggested fix.** Represent running, stopped, missing and unknown separately. H08 gives a bounded Engine-API inspector that distinguishes a successful 404 from an unobservable daemon. Unknown must retain supervision/leases and block destructive operations. Prefer immutable container IDs plus panel/server ownership labels; never use a failed probe as permission for force-removal.

**Implementation excerpt:**

```go
type ContainerState uint8
const (
    ContainerUnknown ContainerState = iota
    ContainerMissing
    ContainerStopped
    ContainerRunning
)
func canReplaceContainer(state ContainerState) error {
    switch state {
    case ContainerMissing, ContainerStopped: return nil
    case ContainerRunning: return fmt.Errorf("container must be stopped gracefully first")
    default: return fmt.Errorf("Docker state unknown; refusing destructive action")
    }
}
// A stopped-state result must come from a successful inspect of the expected
// immutable ID/ownership labels. Inspection errors return Unknown, not Missing.
```

**Required regression verification.** Simulate timeout, permission denial, unavailable daemon, malformed inspect output, genuine missing container and real stopped/running states. Assert unknown never releases active ports, authorizes restore, or triggers rm -f/replacement.

<a id="r36"></a>
### R36. Docker subprocess execution is not uniformly time- and output-bounded

**Priority:** Medium · **Evidence:** Confirmed resource/supervision weakness  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go); [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go); [`internal/arcade/hostcap_darwin.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap_darwin.go)  
**Relevant code:** inspect/info/pull/stats/query subprocesses; CombinedOutput; serialized console work  
**Relationship to prior work:** Known-open A25 centralized executor.

**Problem and impact.** Some Docker operations remain plain exec calls or capture unrestricted output. A stalled inspect/info/query can block a request or a serial worker; a command held under a process mutex can also block stop/control work behind it. Repeated availability checks from polling clients magnify subprocess work. Long-lived `wait`/logs are different: they should live until their owned context ends, not be arbitrarily killed by a short command timeout.

**Suggested fix.** Route finite operations through H01 `BoundedCommand`, with per-operation deadlines and output caps. Cache daemon health briefly and share it across observers. Thread cancellation to the lock/admission layer. Treat pull as a progress-bearing async job and give long followers an application/runner context.

**Implementation excerpt:**

```go
out, err := BoundedCommand(ctx, 10*time.Second, 1<<20,
    "docker", "inspect", "--format", "{{json .State}}", containerID)
if err != nil {
    // Preserve Unknown rather than converting this failure to stopped.
    return fmt.Errorf("inspect unavailable: %w", err)
}
// Example policy: metadata probes 5-10s; commands 15s; graceful stop gets its
// own bounded game-specific timeout; pulls are separate cancelable jobs.
// Do not hold s.mu or a global manager/auth lock while waiting on a process.
```

**Required regression verification.** Use fake executables that never exit, flood stdout/stderr, or leave inherited pipes open. Assert bounded memory/time and cancellation, and verify unrelated servers and stop operations remain responsive.

<a id="r37"></a>
### R37. Container re-adoption does not rebuild the live binding ledger from Docker

**Priority:** High · **Evidence:** Confirmed restart-time reservation gap  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** BindPort/DynamicBindings; Adopt/attach; serverBindings  
**Relationship to prior work:** Residual of pass-5 live binding fix and A24 adoption redesign.

**Problem and impact.** Live BindPort/dynamic-binding state is not persisted and adoption attaches without reconstructing the actual published host bindings. After a live server's desired port was edited and the panel restarts, the container can still occupy its old port while the panel only knows the desired configuration. The previous live/desired union fix therefore does not survive re-adoption. Launch geometry can also differ after a template/configuration change.

**Suggested fix.** Inspect actual Docker PortBindings/NetworkSettings and immutable identity during adoption, build an applied launch snapshot, and reserve the union of actual live bindings and desired future bindings. Refuse or quarantine unknown/inconsistent adoption instead of guessing from current properties.

**Implementation excerpt:**

```go
type PublishedBinding struct {
    HostIP string
    HostPort int
    Protocol string
}
type AppliedRuntime struct {
    ContainerID string
    Bindings []PublishedBinding
    MemoryBytes int64
    CPUs float64
}
// Adopt receives an AppliedRuntime decoded from a successful inspect. Under
// manager -> server lock order, install an immutable copy of actual bindings
// BEFORE publishing running status or permitting another admission.
// A failed inspect keeps the previous/unknown reservation state conservative.
```

**Required regression verification.** Start on port A, set desired port B without restarting, restart only the panel, then attempt another server on A. Admission must still reject the conflict. Include dynamic UDP bindings, wildcard/specific host IPs and template geometry changes.

<a id="r38"></a>
### R38. Port planning is recomputed from a daemon-visible path the panel may not be able to read

**Priority:** Medium · **Evidence:** Confirmed containerized path-space mismatch  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go)  
**Relevant code:** claimStart; dynamicBindings; publishArgs; dockerRunArgs; hostPathFor  
**Relationship to prior work:** Additional integration defect in dynamic port planning.

**Problem and impact.** Admission probes dynamic plugin configuration in the panel-visible tree, but Docker argument generation can repeat the probe using a translated host mount path. In a containerized panel those path spaces need not both exist inside the panel. The port ledger can consequently reserve a dynamic port that the generated docker command omits or maps differently. Two independent probes also race configuration changes.

**Suggested fix.** Build one immutable LaunchPlan while holding the start gate. Read configuration through the panel-visible root exactly once; pass the resulting bindings to both reservation and argument construction. Use the daemon-visible path only for the bind mount source.

**Implementation excerpt:**

```go
type LaunchPlan struct {
    PanelDataPath string
    DaemonMountSource string
    ContainerDataPath string
    Bindings []PublishedBinding
    Env, Args []string
}
func publishPlannedBindings(bindings []PublishedBinding) []string {
    out := make([]string, 0, len(bindings)*2)
    for _, b := range bindings {
        host := strconv.Itoa(b.HostPort)
        if b.HostIP != "" { host = net.JoinHostPort(b.HostIP, host) }
        out = append(out, "-p", host+":"+strconv.Itoa(b.HostPort)+"/"+b.Protocol)
    }
    return out
}
// This helper retains today's equal host/container port model. Extend the
// binding type with ContainerPort before supporting remapped container ports.
```

**Required regression verification.** Run the panel with different inside/outside mount paths. Assert reservation and docker arguments contain exactly the same bindings without reading the daemon path inside the panel. Mutate config after planning and require one coherent plan.

<a id="r39"></a>
### R39. Dynamic plugin port discovery is too permissive and its parser is fragile

**Priority:** Medium · **Evidence:** Confirmed detection/validation weakness  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** Geyser/plugin discovery and configuration parsing  
**Relationship to prior work:** Additional dynamic-binding correctness issue.

**Problem and impact.** Filename-prefix discovery can count disabled jars or non-jar entries as an active plugin. The lightweight line parser also cannot safely stand in for a structured configuration parser across comments/nested sections/invalid ports. Silent fallback can publish/reserve a default port that the game does not use, or reject a legitimate server because a disabled plugin appears to consume a port. These reads also need the bounded rooted I/O from R14.

**Suggested fix.** Require an enabled regular jar and a supported configuration schema. Parse the intended section with a reviewed bounded parser, validate the result, and fail start with a useful configuration error when the plugin is active but its port cannot be determined. Do not silently convert malformed configuration to an assumed default.

**Implementation excerpt:**

```go
func enabledGeyserJar(name string, mode os.FileMode) bool {
    lower := strings.ToLower(name)
    return mode.IsRegular() && strings.HasPrefix(lower, "geyser") &&
        strings.HasSuffix(lower, ".jar") && !strings.HasSuffix(lower, ".jar.disabled")
}
func validDynamicPort(port int) error {
    if port < 1 || port > 65535 { return fmt.Errorf("dynamic plugin port out of range") }
    return nil
}
// Configuration decoding returns (port, error), not a default on parse error.
// Reuse that decoded result in the single LaunchPlan from R38.
```

**Required regression verification.** Test `.jar`, `.jar.disabled`, similarly named folders, inline comments, duplicated/nested port keys, invalid/overflow values, missing config and a configuration change during start admission.

<a id="r40"></a>
### R40. Server identity and directory allocation are not one atomic uniqueness operation

**Priority:** Medium · **Evidence:** Code-confirmed missing uniqueness invariant; rare collision scenario  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go); [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/hub.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hub.go)  
**Relevant code:** ID generation; destination directory creation; registration  
**Relationship to prior work:** Identity hardening; distinguished from already fixed port-lease IDs.

**Problem and impact.** A truncated time/counter ID scheme is not an atomic uniqueness guarantee across all creation paths. Without an exclusive directory/registry claim, a collision can cause two operations to share a destination or replace a registry entry. Reusing a deleted ID is additionally incompatible with retained hub tombstones. This is a rare concurrency/identity risk, not an observed collision in this audit; the fix should establish an invariant rather than depend on timing probability.

**Suggested fix.** Use cryptographically random IDs plus exclusive directory creation and a checked registry claim. Never use MkdirAll to prove a newly allocated ID was unused. Release only the allocating operation's own reservation/staging and never recycle IDs intentionally.

**Implementation excerpt:**

```go
func allocateServerDir(parent string) (id, dir string, err error) {
    for attempt := 0; attempt < 16; attempt++ {
        var raw [16]byte
        if _, err := rand.Read(raw[:]); err != nil { return "", "", err }
        id = hex.EncodeToString(raw[:])
        dir = filepath.Join(parent, id)
        err = os.Mkdir(dir, 0o700) // Exclusive: existing ID is not reused.
        if errors.Is(err, os.ErrExist) { continue }
        if err != nil { return "", "", err }
        return id, dir, nil
    }
    return "", "", fmt.Errorf("could not allocate a unique server directory")
}
// Registry insertion must still check absence under m.mu. Adopt allocates an
// owned staging identity before publishing its external-directory reference.
```

**Required regression verification.** Inject a deterministic colliding ID generator into simultaneous create/import/clone operations. Assert no shared tree, overwritten registry object, duplicate ordering entry or reused dead hub room.

<a id="r41"></a>
### R41. Log supervision does not reconnect, and process-alive is conflated with readiness

**Priority:** Medium · **Evidence:** Confirmed recovery gap plus model improvement  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go)  
**Relevant code:** attach log follower; watchReady fallback; adoption readiness  
**Relationship to prior work:** Known-open A26 reconnect; readiness-model improvement.

**Problem and impact.** After a log-follow process terminates, reporting its error does not restore the console stream while the container remains alive. Separately, the readiness fallback eventually calls a still-running container running even if the game is still downloading/generating or has never become ready. The warning is useful, but consumers have no separate readiness/observability state and can make unsafe assumptions from running status alone.

**Suggested fix.** Reconnect log following with bounded backoff, a timestamp cursor/overlap policy and duplicate handling. Represent process state, readiness and log-stream health separately. A readiness timeout should produce running-but-readiness-unknown/degraded, not fabricated verified readiness. Keep cancellation and immutable container identity attached to the watcher generation.

**Implementation excerpt:**

```go
type RuntimeHealth struct {
    ProcessRunning bool `json:"process_running"`
    Ready *bool `json:"ready"` // nil means unverified/unknown, not false success.
    LogsConnected bool `json:"logs_connected"`
    LastObserved time.Time `json:"last_observed"`
}
func reconnectDelay(attempt int) time.Duration {
    if attempt < 0 { attempt = 0 }
    if attempt > 5 { attempt = 5 }
    return time.Second * time.Duration(1<<attempt)
}
// Follower loop: follow immutable ID from cursor; on transport failure set
// LogsConnected=false, WaitContext(backoff), re-inspect, reconnect. Stop on
// confirmed exit or context cancellation; preserve ready=nil if never verified.
```

**Required regression verification.** Restart/hiccup the daemon while a game stays alive, interrupt only the log process, and boot a game slower than the fallback. Assert reconnection without duplicate floods and honest process/readiness/log health.

<a id="r42"></a>
### R42. Lifecycle requests need durable desired state and generation fencing

**Priority:** Medium · **Evidence:** Concurrency/design risk corroborated by lifecycle structure  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** Restart workers; delayed auto-resume/reconcile; start/stop acknowledgments  
**Relationship to prior work:** Related to A24/A32/A33 lifecycle redesign.

**Problem and impact.** Delayed workers can act on a previously captured intent after a newer stop/restart request. A status enum and an asynchronous acknowledgment do not identify which operation owns the next start. Auto-resume after a Docker outage must not resurrect a server the operator subsequently wanted stopped. Repeated restart requests also need coalescing or ordering rather than independent background sequences.

**Suggested fix.** Persist DesiredRunning and increment an operation generation under the lifecycle lock. Every delayed effect rechecks both generation and intent immediately before acting. Return an operation ID and separate accepted/completed states. H07 can execute the work but generation fencing belongs in the lifecycle manager.

**Implementation excerpt:**

```go
type LifecycleIntent struct {
    Generation uint64 `json:"generation"`
    DesiredRunning bool `json:"desired_running"`
}
func mayStart(captured uint64, current LifecycleIntent) bool {
    return captured == current.Generation && current.DesiredRunning
}
// A stop request increments Generation and durably publishes false before
// canceling/waiting on older work. A restart captures its own new generation.
// Auto-resume and scheduler workers must check mayStart at effect time, not
// only when they were enqueued. Unknown Docker state still blocks the effect.
```

**Required regression verification.** Barrier-test restart→stop, auto-resume→stop during daemon outage, two restarts, delete during restart and panel restart with a pending intent. The newest durable intent must win.

<a id="r43"></a>
### R43. A metrics sampler and several snapshots still expose mutable state without a complete locked copy

**Priority:** Medium · **Evidence:** Confirmed data-race/snapshot defects; full race suite not run  
**Source:** [`internal/arcade/metrics.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/metrics.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** sampleLoop; PendingRestart snapshots; worker input snapshots  
**Relationship to prior work:** Residual synchronization beyond earlier A20/Props fixes.

**Problem and impact.** In sampleLoop, CPU usage is read under s.mu but the later aggregation reads `s.CPU` after unlocking; concurrent resource updates can race with it. Mutable slices/maps/pointers passed into asynchronous work or returned as snapshots also need deep-copy discipline. A copied slice header is not a copy of its backing array—proof 10 demonstrates that primitive. Prior Props-locking fixes should not be re-reported as absent; the remaining accesses need targeted tests.

**Suggested fix.** Copy every needed field under the owning lock and use only that immutable snapshot afterwards. Deep-copy map/slice members of API and job snapshots. Scheduler dispatch should capture task values under its lock rather than pass a live task pointer to a worker.

**Implementation excerpt:**

```go
// In sampleLoop:
s.mu.Lock()
cpu, mem, players := s.cpuPct, s.memMB, len(s.players)
quota := s.CPU
running := s.Status == StatusRunning
s.mu.Unlock()
// Use quota, never s.CPU, below this point (also correct units per R44).
_ = quota
// For returned snapshots, while locked:
pending := append([]string(nil), s.PendingRestart...)
props := copyProps(s.Props)
// For scheduler worker input: copy the Task value plus any slice/map members
// under Scheduler.mu, then release the lock before executing it.
_, _ = pending, props
```

**Required regression verification.** Add race tests concurrently changing resources while sampling, mutating pending keys while serializing snapshots, editing tasks during dispatch, and changing source configuration during clone. Run these inside the real package with `go test -race`.

<a id="r44"></a>
### R44. CPU percentages are mixed across units, and memory-unit parsing is incomplete

**Priority:** Medium · **Evidence:** Confirmed metric interpretation bug; isolated arithmetic proof  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/metrics.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/metrics.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go)  
**Relevant code:** Docker stats parsing; Sample.CPU; host CPU aggregation; parseMem  
**Relationship to prior work:** Additional metrics correctness issue.

**Problem and impact.** Docker's CLI CPU percentage uses CPU time relative to a core-based host accounting formula, not the server's configured CPU quota as denominator. Treating it as quota-relative and multiplying by s.CPU overstates aggregate consumption for multi-CPU quotas. The current desired quota may also differ from the running container's quota. Memory text parsing needs explicit unit handling: bytes, SI and IEC suffixes must not fall through to incompatible assumed megabytes. Proof 11 isolates the CPU-unit mismatch.

**Suggested fix.** Store numeric used cores and bytes from the Engine stats API where possible. Derive display percentages from the actual applied quota, not desired settings. If retaining CLI parsing, parse every supported unit explicitly and return unknown/error on unsupported text rather than a plausible wrong number.

**Implementation excerpt:**

```go
func cpuMetrics(dockerPercent, appliedQuota float64) (usedCores, quotaPercent float64) {
    usedCores = dockerPercent / 100
    if appliedQuota > 0 { quotaPercent = 100 * usedCores / appliedQuota }
    return
}
var memoryUnit = map[string]float64{
    "B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
    "KiB": 1<<10, "MiB": 1<<20, "GiB": 1<<30, "TiB": 1<<40,
}
// Parse numeric prefix + exact suffix; reject NaN/Inf/negative/overflow.
// hostUsedCores = sum(usedCores); per-server % = quotaPercent.
// Simulator metrics must adopt the same explicit internal units.
```

**Required regression verification.** Test 100% Docker CPU with quotas 0.5, 1, 2 and 4, plus a pending quota change. Test bytes/KiB/MiB/GiB and SI variants, malformed/overflow input, and aggregation without double multiplication.

**Primary external reference:** [Docker stats API accounting](https://docs.docker.com/reference/api/engine/version/v1.46/) and [Docker stats CLI](https://docs.docker.com/reference/cli/docker/container/stats/).

<a id="r45"></a>
### R45. Host capacity and utilization can describe different resource scopes

**Priority:** Medium · **Evidence:** Confirmed deployment-dependent metrics/admission mismatch  
**Source:** [`internal/arcade/hostcap_linux.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap_linux.go); [`internal/arcade/hostcap.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** memTotalMB; memUsedMB; Host; capacity initialization  
**Relationship to prior work:** Additional host observability/admission issue.

**Problem and impact.** Linux memory capacity takes the minimum of /proc and the panel's cgroup ceiling, but memory used is derived from /proc host values. Numerator and denominator can therefore describe different scopes. A containerized panel controlling sibling game containers has another mismatch: its own cgroup limit is not the daemon host's total game capacity. Remote Docker/macOS VM arrangements similarly invalidate an assumption that the panel process's machine is the game host. Failed/early disk probes should not become permanent invented capacity data.

**Suggested fix.** Model panel-process resources and game-runtime-host resources separately, including source/freshness/unknown state. Query the daemon/runtime host for game admission and use matching scoped usage counters. Re-probe disk capacity after creating the data directory and when mount availability changes; unknown is not zero usage or unlimited capacity.

**Implementation excerpt:**

```go
type CapacityReading struct {
    Scope string `json:"scope"` // e.g. panel-cgroup or docker-daemon-host
    Source string `json:"source"`
    MemoryTotalBytes *uint64 `json:"memory_total_bytes"`
    MemoryUsedBytes *uint64 `json:"memory_used_bytes"`
    ObservedAt time.Time `json:"observed_at"`
}
func comparableCapacity(totalScope, usageScope string) bool {
    return totalScope != "" && totalScope == usageScope
}
// Refuse to compute percentages or enforce host-fit from mismatched scopes.
// Obtain daemon capacity from a successful runtime-host capability probe;
// do not substitute the panel cgroup when managing sibling containers.
```

**Required regression verification.** Test a low-memory panel container controlling a larger daemon host, cgroup-limited native/LXC deployment, unknown probes, delayed data-directory creation and remote Docker. Percentages and admission must use the same documented scope.

<a id="r46"></a>
### R46. The minimum Java heap can consume the entire allowed container memory

**Priority:** Medium · **Evidence:** Confirmed resource-sizing edge case  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** Java heap computation; memory limit validation  
**Relationship to prior work:** Additional effective-resource validation edge case.

**Problem and impact.** The accepted lower memory range overlaps a heap-floor calculation that can select a 512 MiB heap for a 512 MiB container. That leaves no intended allowance for native JVM memory, threads, buffers or other processes. This is a predictable sizing risk, not a guarantee that every such container immediately OOMs. Non-Java games should not inherit a Java-specific rule.

**Suggested fix.** Validate an effective Java memory budget, including a minimum native reserve, before Create/Import/Clone/resource updates. Make heap and container limits explicit and keep the default conservative. Do not silently clamp a heap upward beyond the admitted budget.

**Implementation excerpt:**

```go
func javaHeapMB(containerMB int) (int, error) {
    const minimumContainer = 1024
    const nativeReserve = 512
    if containerMB < minimumContainer {
        return 0, fmt.Errorf("Java servers require at least %d MB", minimumContainer)
    }
    heap := containerMB-nativeReserve
    if proportional := containerMB*3/4; proportional < heap { heap = proportional }
    return heap, nil
}
// Apply only to Java-server capabilities, not every game. Large modpacks may
// require a larger configured reserve; expose the effective heap in the UI.
```

**Required regression verification.** Test values below/at/above the minimum and verify heap < container limit with the documented reserve. Smoke-test representative Java images and retain independent sizing for non-Java templates.

<a id="r47"></a>
### R47. The Rust template can expose the upstream image’s known default RCON password

**Priority:** High · **Evidence:** Confirmed template/upstream contract; deployment conditional  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** Rust template Protocols/PortSpan; dockerRunArgs non-itzg environment  
**Relationship to prior work:** Newly identified template integration/security issue; upstream source linked in external references.

**Problem and impact.** The Rust template publishes TCP and UDP across its two-port span. At the default game port this includes the image's RCON TCP port. The generated per-launch RCON secret is wired through the itzg-specific path, not this Rust image. The upstream didstopia image source reviewed during this audit sets `RUST_RCON_PASSWORD` to the known default `docker`. Consequently the default template/image contract can expose a predictable RCON credential. The live digest actually deployed was not available, so verify running containers as well as fixing the template.

**Suggested fix.** Never publicly publish control ports by default, and inject a fresh generated secret using the image's actual variable. Audit existing running containers because changing a template does not rotate an existing secret or remove existing bindings. Tie this to a verified Rust launch adapter, not a static secret in a JSON template.

**Implementation excerpt:**

```go
// In dockerRunArgs, BEFORE appending the image/command, after ordinary
// template environment expansion. `secret` is the existing random secret.
if s.Game == "rust" {
    args = append(args, "-e", "RUST_RCON_PASSWORD="+secret)
}
// Rust network policy: publish only required game/query UDP bindings.
// Do NOT publish RCON's TCP binding by default. Keep its internal port explicit.
// Example template env (non-secret):
// RUST_SERVER_PORT=${PORT}
// RUST_SERVER_QUERYPORT=${PORT_PLUS_1}
// RUST_RCON_PORT=28016
// Runtime-generated RUST_RCON_PASSWORD overrides any default.
// Do not log secrets in complete docker argument dumps.
```

**Required regression verification.** Inspect generated arguments and an actual running pinned image. Assert no default password, no unintended public RCON TCP binding, distinct secrets across starts, correct game/query ports and no secret in API/audit output.

**Primary external reference:** [Reviewed upstream Rust Dockerfile](https://github.com/didstopia/rust-server/blob/master/Dockerfile) and [image documentation](https://hub.docker.com/r/didstopia/rust-server/). These upstream references are not pinned to the audited Arcade commit or a deployed image digest.

<a id="r48"></a>
### R48. Rust and Valheim templates do not fully describe their persistent paths and launch settings

**Priority:** High · **Evidence:** Confirmed template/upstream contract mismatch  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** Rust/Valheim template definitions; serverDataPath; templateLaunchArgs  
**Relationship to prior work:** Newly identified incomplete native-image contracts.

**Problem and impact.** Rust and Valheim lack complete per-image DataPath/environment mappings, while the generic fallback is /data and non-itzg images do not receive the itzg settings. The reviewed Rust image stores its server data under /steamcmd/rust; the Valheim image documents /config for persistent configuration/worlds. Mounting the wrong directory can make panel files/backups unrelated to the real world and lose container-local data on recreation. Port/player/name/password settings also need each image's actual contract. These templates being preview does not make world persistence optional.

**Suggested fix.** Disable creation from unverified templates until persistence/network/secret contract tests pass. Then ship explicit paths and environment mappings, migrate existing installs without deleting container-local worlds, and expose required secret inputs rather than invented defaults. A template update alone must not recreate a running world before migration.

**Implementation excerpt:**

```go
// Required template corrections, alongside a verified adapter:
// Rust:
//   DataPath: "/steamcmd/rust"
//   Env: RUST_SERVER_PORT=${PORT}, RUST_SERVER_NAME=${MOTD},
//        RUST_SERVER_MAXPLAYERS=${MAX_PLAYERS}; query/RCON handled per R47.
// Valheim:
//   DataPath: "/config"
//   Env: SERVER_PORT=${PORT}, SERVER_NAME=${MOTD}, WORLD_NAME=<explicit choice>
//   SERVER_PASS: supplied through a secret field, not a template default.
func requireVerifiedTemplate(verified bool, slug string) error {
    if !verified { return fmt.Errorf("template %s is disabled until persistence and launch contracts are verified", slug) }
    return nil
}
// Add verified capability metadata; check it at every creation/import launch
// admission, with a deliberate migration override for existing installations.
```

**Required regression verification.** For each pinned image: create a sentinel/world, stop, recreate the container, then back up/restore and confirm the game loads the same world. Test non-default ports/settings and required secrets. Include an existing container with world data only in its writable layer.

**Primary external reference:** [Rust image contract](https://hub.docker.com/r/didstopia/rust-server/) and [Valheim image contract](https://hub.docker.com/r/lloesche/valheim-server). Verify the exact image digest before migration.

<a id="r49"></a>
### R49. Bedrock configures a second listener without declaring the full port geometry

**Priority:** Medium · **Evidence:** Confirmed template inconsistency  
**Source:** [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)  
**Relevant code:** Bedrock Env SERVER_PORT_V6; PortSpan; candidateBindings  
**Relationship to prior work:** Additional template geometry omission.

**Problem and impact.** The Bedrock environment assigns its second listener `PORT_PLUS_1`, but the template does not declare the corresponding full span for reservation/publication. The ledger therefore need not protect every configured port, and a base of 65535 creates an invalid second port. Correct publication still depends on the host/Docker IPv6 configuration; adding a port does not itself guarantee IPv6 reachability.

**Suggested fix.** Represent every configured listener in the template's typed geometry and validate expansion before admission. For the existing equal-port model, a two-port UDP span is the localized correction; then test the image's actual IPv4/IPv6 binding behavior.

**Implementation excerpt:**

```go
// Bedrock template correction:
Protocols: []string{"udp"},
PortSpan: 2,
Env: map[string]string{"SERVER_PORT_V6": "${PORT_PLUS_1}"},
// Existing candidateBindings must validate base+span-1 <= 65535.
// Reject base 65535, and ensure claim, start, clone, import, settings changes
// and re-adoption all reserve/publish the same two intended UDP bindings.
```

**Required regression verification.** Test base 65534 and 65535, a conflicting server on the second UDP port, clone/import admission, and actual reachability with documented Docker IPv6 settings.

<a id="r50"></a>
### R50. Template seeding can overwrite customized files when its ledger is missing or corrupt

**Priority:** Medium · **Evidence:** Confirmed migration/durability risk  
**Source:** [`internal/arcade/templates.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/templates.go)  
**Relevant code:** syncSeededTemplates; .seeded.json; .superseded writes  
**Relationship to prior work:** Additional template update ownership/durability issue.

**Problem and impact.** Template files and the seed ledger are written directly rather than atomically/durably. A corrupt or missing ledger is treated as an older installation and permits replacing a customized template while writing a fixed-name `.superseded` copy. Repeated uncertain migrations can overwrite that backup. The logging and preservation attempt are helpful, but an unreadable ownership ledger is not positive evidence that a file is safe to replace.

**Suggested fix.** Treat unknown ownership conservatively: retain the active custom file, write the new built-in to a no-replace candidate, and require an explicit migration choice. Use durable atomic writes for ledger and managed files, publish the ledger after successful file updates, and never overwrite an earlier recovery copy.

**Implementation excerpt:**

```go
func writeCandidateNoReplace(path string, body []byte) (err error) {
    f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
    if err != nil { return err }
    defer func() { if closeErr := f.Close(); err == nil { err = closeErr } }()
    if _, err = f.Write(body); err != nil { return err }
    return f.Sync()
}
// Unknown/corrupt ledger: do NOT replace slug.json. Write a uniquely named
// reviewed-update candidate; sync its parent too before recording its presence.
// Known-owned update: use AtomicStateWrite and publication-aware outcomes.
// Never reuse a fixed .superseded filename for destructive migration backup.
```

**Required regression verification.** Corrupt/truncate the ledger, interrupt an update halfway through the template set, and restart repeatedly with a custom template. Its active content and every prior recovery copy must survive unchanged.

<a id="r51"></a>
### R51. Template validation does not validate the launch contract or catalog uniqueness

**Priority:** Medium · **Evidence:** Confirmed validation omission  
**Source:** [`internal/arcade/templates.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/templates.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go)  
**Relevant code:** validateTemplate; LoadTemplates; template lookup/fallback  
**Relationship to prior work:** Additional template correctness/operability improvement.

**Problem and impact.** Validation checks a few required fields and positive defaults, but not unique slugs across files, complete port/protocol geometry, supported capabilities, safe/meaningful container data paths, required secret inputs or recognized expansion variables. Duplicate slugs make lookup depend on ordering rather than an unambiguous identity. Publishing a partly valid catalog/falling back to built-ins also needs an explicit operator-visible policy rather than silently changing the available launch definition. The same capability model should gate Minecraft-specific properties/player-list actions for non-Minecraft templates rather than presenting unsupported actions as generic features.

**Suggested fix.** Validate the complete catalog into an immutable candidate before publishing it. Reject duplicate identities and unsupported geometry/capability contracts, and distinguish optional invalid custom additions from a missing/corrupt critical built-in. Do not infer game capabilities solely from an image-name prefix.

**Implementation excerpt:**

```go
func validateCatalogIdentity(templates []Template) error {
    seen := make(map[string]struct{}, len(templates))
    for _, t := range templates {
        if _, ok := seen[t.Slug]; ok { return fmt.Errorf("duplicate template slug %q", t.Slug) }
        seen[t.Slug] = struct{}{}
        if t.PortSpan < 0 { return fmt.Errorf("negative port span: %s", t.Slug) }
        for _, p := range t.Protocols {
            if p != "tcp" && p != "udp" { return fmt.Errorf("invalid protocol %q in %s", p, t.Slug) }
        }
        if t.DataPath != "" && !path.IsAbs(t.DataPath) { return fmt.Errorf("container data path must be absolute") }
    }
    return nil
}
// Also call candidateBindings on effective defaults and validate each known
// placeholder/capability/required secret before atomically swapping the catalog.
```

**Required regression verification.** Load duplicate slugs, bad protocols, overflowing spans, unknown variables, relative data paths, absent required secrets and invalid capabilities. Confirm the previous valid catalog remains active or startup fails clearly, never an unexplained partial replacement.

## Players, scheduling and frontend

<a id="r52"></a>
### R52. Player-list edits can discard fields and do not establish a complete identity contract

**Priority:** Medium · **Evidence:** Confirmed lossy model; UUID behavior requires game integration verification  
**Source:** [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go); [`cmd/teploy-arcade/frontend/views2.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views2.js)  
**Relevant code:** ListEntry JSON decoding/writing; stopped-server list additions  
**Relationship to prior work:** Additional player-list data preservation and integration issue.

**Problem and impact.** Decoding game-owned list files into a reduced struct and rewriting the entire array drops fields the panel does not model. Concurrent panel edits can also overwrite each other without a shared edit transaction. For stopped-server name-only additions, the UI accepts a name/UUID but the backend needs an explicit name-to-UUID/online-mode policy; it should not assume every game will fill in a missing identifier later. The precise effect of an empty UUID was not boot-tested against a game image in this audit, so that part is an integration risk, not a claimed reproduced game failure.

**Suggested fix.** Preserve unknown entry fields, use rooted bounded reads and the same edit/ETag transaction as the file editor, and validate the selected list schema. Resolve a real UUID through a declared online/offline-mode adapter or reject unsupported stopped name-only additions. Validate banned IPs with netip, not only string filtering.

**Implementation excerpt:**

```go
type PlayerListRecord map[string]json.RawMessage
func setRecordString(r PlayerListRecord, key, value string) error {
    b, err := json.Marshal(value)
    if err != nil { return err }
    r[key] = b
    return nil
}
func validateStoppedPlayerIdentity(uuid string) error {
    compact := strings.ReplaceAll(uuid, "-", "")
    b, err := hex.DecodeString(compact)
    if err != nil || len(b) != 16 { return fmt.Errorf("a resolved UUID is required while the server is stopped") }
    return nil
}
// Retain unknown RawMessage fields on unaffected records. Change only the
// intended entry under editMu, with a byte cap and generation/ETag check.
```

**Required regression verification.** Round-trip a record containing an unknown extension field and an operator-specific flag. Test simultaneous list/file edits, malformed JSON/FIFO/oversized files, online/offline identity resolution and actual loading by each supported game version.

<a id="r53"></a>
### R53. Player metadata and unanchored log matches can look more authoritative than they are

**Priority:** Low · **Evidence:** Confirmed observability weakness  
**Source:** [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go); [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go); [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go)  
**Relevant code:** trackPlayer; join/login regexes; placeholder player fields  
**Relationship to prior work:** Additional observability improvement.

**Problem and impact.** Player objects can carry placeholder/synthetic UUID or ping values without a reliable measurement/source marker. Loose log substring matching can interpret text resembling join/leave messages as authoritative player events, including plugin/chat text. Reconciliation helps supported games, but it is delayed and not universal. These are misleading operational data, not a demonstrated authentication bypass.

**Suggested fix.** Represent unknown measurements as null and attach source/freshness. Anchor recognized log grammar after a verified game-log prefix and prefer a typed roster query. Never expose simulator placeholders as measured production values. Bound player cardinality and reject malformed names.

**Implementation excerpt:**

```go
type PlayerObservation struct {
    Name string `json:"name"`
    UUID *string `json:"uuid"`
    PingMS *int `json:"ping_ms"`
    Source string `json:"source"`
    ObservedAt time.Time `json:"observed_at"`
}
// A parsed join message supplies Name/Source/ObservedAt, not a fabricated ping.
// Only a verified roster/telemetry adapter fills UUID/PingMS. Frontend renders
// null as unknown. Anchor joins to the supported log grammar, excluding chat.
```

**Required regression verification.** Feed player chat containing `OtherPlayer joined the game`, plugin-prefixed messages and genuine server events. Confirm only trusted grammar changes the roster, and unavailable ping/UUID is displayed as unknown.

<a id="r54"></a>
### R54. Due scheduled tasks are dropped when four long jobs occupy the run slots

**Priority:** Medium · **Evidence:** Confirmed scheduling/admission bug  
**Source:** [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** scheduler tick window; run slots; RunNow  
**Relationship to prior work:** Incomplete A32 slot-bound improvement.

**Problem and impact.** The scheduler checks a narrow current-time window and only starts work when one of four slots is free. A fifth due task can leave the window while the first four run, then never execute that occurrence. A concurrency limit should queue/reject explicitly, not silently redefine the schedule. Manual RunNow also needs the same global admission policy rather than a separate unlimited path.

**Suggested fix.** Persist due occurrences into a pending queue and dispatch them through H07's bounded workers. Calculate due occurrences between the last successful scan and now under a documented catch-up policy. Bound backlog and report overload visibly; do not silently drop a promised backup/restart.

**Implementation excerpt:**

```go
type Occurrence struct {
    TaskID string `json:"task_id"`
    ScheduledFor time.Time `json:"scheduled_for"`
    State string `json:"state"` // pending/running/succeeded/failed/unknown
}
func occurrenceKey(taskID string, scheduled time.Time) string {
    return taskID + ":" + scheduled.UTC().Format(time.RFC3339Nano)
}
// On tick: compute missed/due occurrences, persist unique pending records,
// then dispatch pending work when capacity exists. Queue-full stays pending
// with an overload signal. RunNow produces an explicit manual occurrence and
// uses the same worker/admission budget.
```

**Required regression verification.** Schedule at least five jobs for one instant while four hold slots beyond the minute. Every due occurrence must be queued or explicitly reported missed per policy. Include panel downtime, clock jumps and a burst of manual runs.

<a id="r55"></a>
### R55. Scheduler preview, completion bookkeeping and persistence do not define the same occurrence

**Priority:** Medium · **Evidence:** Confirmed scheduler state inconsistency  
**Source:** [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** NextRun; due calculation; record; last-run persistence  
**Relationship to prior work:** Residual beyond A31 civil-time fix; state-store work also applies.

**Problem and impact.** NextRun and the execution loop disagree for some missed one-shot schedules: a preview can report no next run while a later day can still execute it. Completion-time LastRun is also not the same as the occurrence's scheduled date, particularly for a task running across midnight. Bookkeeping that mutates memory before persistence and suppresses a record-write failure can report a successful durable run when the restart state disagrees.

**Suggested fix.** Give every scheduled/manual run an explicit occurrence ID and scheduled timestamp. Derive preview and execution from one function. Persist claim and completion states with publication-aware errors. Choose and document at-most-once/manual-recovery versus retry behavior for ambiguous effects; arbitrary console commands cannot be made exactly-once by a JSON counter.

**Implementation excerpt:**

```go
type TaskRunRecord struct {
    ID, TaskID string
    ScheduledFor, StartedAt, FinishedAt time.Time
    State, Error string
}
func nextOneShot(now, scheduled time.Time, completed bool) *time.Time {
    if completed { return nil }
    // Explicit catch-up policy: pending past occurrences remain due NOW,
    // rather than pretending they are a new occurrence tomorrow.
    if scheduled.Before(now) { due := now; return &due }
    due := scheduled; return &due
}
// One civil-time/DST resolver supplies scheduled occurrences to BOTH preview
// and dispatch. Store completion separately; propagate record-write failures.
```

**Required regression verification.** Test missed one-shots, disabled/re-enabled tasks, midnight-crossing executions, DST gaps/repeated times, panel restart during execution and completion-record write failure. Preview must match the next executable occurrence.

<a id="r56"></a>
### R56. Task steps have no coherent cancellation and treat lifecycle acceptance as completion

**Priority:** Medium · **Evidence:** Confirmed execution-contract gap  
**Source:** [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)  
**Relevant code:** task command steps; !wait; asynchronous lifecycle calls  
**Relationship to prior work:** Known-open A32 context/cancellation; additional completion semantics.

**Problem and impact.** Task steps can sleep for long periods without application/request cancellation. Start/Stop/Restart are asynchronous, so accepting one does not mean the next task command is safe to send. Deleting/disabling a task while it runs also lacks a clear cancellation contract. Semicolon splitting cannot unambiguously represent game commands that legitimately contain semicolons.

**Suggested fix.** Use typed steps, an owned context and explicit operation-completion waits. H01 `WaitContext` replaces non-cancelable sleeps. Persist cancellation/partial outcomes and state whether disabling affects future occurrences only or also cancels an active one. Existing text syntax can remain as a validated compatibility parser, not the canonical stored form.

**Implementation excerpt:**

```go
type TaskStep struct {
    Kind string `json:"kind"`
    Argument string `json:"argument,omitempty"`
    WaitSeconds int `json:"wait_seconds,omitempty"`
}
func executeSteps(ctx context.Context, steps []TaskStep,
    executeAndWait func(context.Context, TaskStep) error) error {
    for _, step := range steps {
        if err := ctx.Err(); err != nil { return err }
        if step.Kind == "wait" {
            if step.WaitSeconds < 0 || step.WaitSeconds > 900 { return fmt.Errorf("wait out of range") }
            if err := WaitContext(ctx, time.Duration(step.WaitSeconds)*time.Second); err != nil { return err }
        } else if err := executeAndWait(ctx, step); err != nil { return err }
    }
    return nil
}
// executeAndWait resolves lifecycle operation IDs and waits for their actual
// terminal state. It must not return merely because Start/Stop accepted work.
```

**Required regression verification.** Run restart→console, stop→backup and start→ready-dependent command sequences. Cancel during wait and lifecycle work; test disable/delete policy and literal semicolons in a typed console step.

<a id="r57"></a>
### R57. Editing a disabled task re-enables it, and toggle requests overwrite concurrent edits

**Priority:** Medium · **Evidence:** Confirmed; isolated JavaScript proofs  
**Source:** [`cmd/teploy-arcade/frontend/views2.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views2.js); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)  
**Relevant code:** taskDialog save body; scheduler toggle PATCH  
**Relationship to prior work:** New frontend regression after backend pointer-PATCH improvement.

**Problem and impact.** The edit dialog always sends enabled:true. Renaming a disabled scheduled task therefore reactivates it without an explicit enable action. The toggle handler sends a stale full task object, so toggling enabled can overwrite a concurrent edit to its name/time/commands. Proofs 14 and 17 reproduce these request-shape consequences.

**Suggested fix.** Send only changed fields. Set enabled for new tasks, but preserve it on edits unless the dialog has a deliberate enable control. Toggle with only enabled plus an optimistic revision token. Coordinate this with strict unknown-field decoding.

**Implementation excerpt:**

```javascript
// taskDialog's save handler:
const body = {
  name: $('#tName', modal).value.trim(),
  commands: $('#tCmd', modal).value.trim(),
  time: $('#tTime', modal).value.trim(),
  repeat: rep.classList.contains('is-on'),
};
if (!editing) body.enabled = true;
// Toggle handler: do not spread the full stale task into the PATCH.
await api(`/api/servers/${id}/tasks/${tid}`, {
  method: 'PATCH',
  body: JSON.stringify({ enabled: !task.enabled }),
  headers: { 'If-Match': task.etag }, // Added by the revision-aware API.
});
```

**Required regression verification.** Browser-test editing a disabled task and toggling a task while another client changes its commands. Enabled remains false after an unrelated edit; stale revisions cause a conflict rather than overwriting content.

<a id="r58"></a>
### R58. A slower route request can overwrite a newer navigation

**Priority:** Medium · **Evidence:** Confirmed; isolated JavaScript ordering proof  
**Source:** [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js); [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js); [`cmd/teploy-arcade/frontend/views2.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views2.js)  
**Relevant code:** async route; view factories; timers and teardown  
**Relationship to prior work:** Newly identified client concurrency bug.

**Problem and impact.** The router awaits asynchronous view construction without a generation/abort guard. Navigate from A to B while A's fetch is slow and A can mount after B, displaying the wrong server/page. Discarded views can also create timers or subscriptions unless teardown is delivered. Similar out-of-order fetches exist within polling/list views. Proof 12 demonstrates the stale-mount ordering.

**Suggested fix.** Assign each navigation a generation and AbortController, and mount only the current generation. Teardown discarded candidates and the prior mounted view; pass AbortSignal into view fetches. Serialize polling rather than overlapping setInterval requests, and cancel pending work on teardown.

**Implementation excerpt:**

```javascript
let routeGeneration = 0;
let routeAbort;
async function renderLatest(makeView) {
  const generation = ++routeGeneration;
  routeAbort?.abort();
  const controller = new AbortController();
  routeAbort = controller;
  let candidate;
  try { candidate = await makeView(controller.signal); }
  catch (error) {
    if (controller.signal.aborted || generation !== routeGeneration) return;
    throw error;
  }
  if (controller.signal.aborted || generation !== routeGeneration) {
    candidate?.dispatchEvent(new Event('gss:teardown'));
    return;
  }
  const host = document.querySelector('#view');
  host.firstElementChild?.dispatchEvent(new Event('gss:teardown'));
  host.replaceChildren(candidate);
}
// View requests accept signal; each controller/timer observes teardown.
```

**Required regression verification.** Use controllable promises for A/B/C navigation and delayed errors. Assert the newest route always wins, stale views are torn down, and no timer/WebSocket remains for an unmounted server.

<a id="r59"></a>
### R59. Session expiration and dynamic role changes do not have one client-side state transition

**Priority:** Medium · **Evidence:** Confirmed client lifecycle/affordance weakness  
**Source:** [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js); [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js)  
**Relevant code:** api wrapper; event/console reconnect; permission observer; modal controls  
**Relationship to prior work:** Additional client auth lifecycle improvement.

**Problem and impact.** The API wrapper reduces HTTP failures to generic errors and does not centrally transition an expired session back to sign-in. SSE/WebSocket reconnect logic can continue retrying while authentication is no longer valid. Cached role-based disabling is also applied inconsistently to dynamically rebuilt controls and modal content; connection/busy handlers can overwrite disabled state. The backend remains the security boundary, so these UI errors are not themselves privilege escalation.

**Suggested fix.** Preserve HTTP status/error codes, centralize auth-required/must-change transitions, stop live connections while signed out, and refresh capabilities after role changes. Derive disabled state from permission AND connectivity AND busy state instead of independent handlers assigning it. Apply the same logic to modal controls.

**Implementation excerpt:**

```javascript
class APIError extends Error {
  constructor(status, message, payload) { super(message); this.status = status; this.payload = payload; }
}
async function requestJSON(url, options = {}) {
  const res = await fetch(url, { credentials: 'same-origin', ...options });
  const payload = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (res.status === 401) window.dispatchEvent(new Event('arcade:auth-required'));
    throw new APIError(res.status, payload.error || `HTTP ${res.status}`, payload);
  }
  return payload;
}
function setActionState(button, { allowed, connected = true, busy = false }) {
  button.disabled = !allowed || !connected || busy;
}
// One app-owned auth coordinator handles the event idempotently: abort feeds,
// enter sign-in, clear cached capabilities, then resume only after success.
```

**Required regression verification.** Expire/revoke a session while pages, modals and consoles are open; assert one sign-in transition and stopped reconnect loops. Change roles and exercise freshly rebuilt header/modal buttons. Backend denial must remain intact.

<a id="r60"></a>
### R60. The Kick action inherits chat mode and may broadcast text instead of kicking

**Priority:** Medium · **Evidence:** Confirmed; isolated JavaScript proof  
**Source:** [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js)  
**Relevant code:** Console.renderPlayers; sendRaw; chat mode  
**Relationship to prior work:** Newly identified UI action bug.

**Problem and impact.** The player Kick button calls sendRaw with a kick command, but sendRaw selects mode from the console's current chat toggle. In chat mode this sends a say-mode message rather than the administrative command. Proof 13 reproduces the selection. Administrative controls must not depend on an unrelated text-input mode.

**Suggested fix.** Make command mode explicit for typed administrative actions. Keep chat mode only for the free-text send button, validate/encode the player identity appropriately, and retain text until acknowledgment or offer a retry on rejection. Use collision-resistant client command IDs rather than timestamps alone.

**Implementation excerpt:**

```javascript
sendRaw(text, { mode = this.chat ? 'say' : 'command' } = {}) {
  if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
    toast('Not connected', 'err'); return;
  }
  this.ws.send(JSON.stringify({
    t: 'command', id: crypto.randomUUID(), text, mode,
  }));
}
// Kick binding:
b.addEventListener('click', () => {
  const name = b.dataset.kick;
  if (!/^[A-Za-z0-9_]{3,16}$/.test(name)) { toast('Invalid player name', 'err'); return; }
  this.sendRaw(`kick ${name}`, { mode: 'command' });
});
// Non-Minecraft adapters need their own valid player-identifier encoding.
```

**Required regression verification.** With chat mode enabled, click Kick and assert the WebSocket message uses command mode and the runtime action occurs. Test disconnected/rejected acknowledgments and unsupported player-name formats.

<a id="r61"></a>
### R61. Import UI can select adoption implicitly and retain a runtime different from the displayed choice

**Priority:** Medium · **Evidence:** Confirmed client intent mismatch  
**Source:** [`cmd/teploy-arcade/frontend/views-import.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-import.js)  
**Relevant code:** renderScan; setMode; runtime selection; scan/poll lifecycle  
**Relationship to prior work:** Newly identified import client intent issues.

**Problem and impact.** Insufficient copy space automatically selects adopt, turning a capacity problem into an in-place-management choice that can modify an external directory. On rescan, the controls can visually show Simulator while the runtime variable retains a previous Docker selection. A scan or route change during polling can also leave responses updating removed/currently unrelated controls. These are operator-intent bugs around a potentially destructive workflow.

**Suggested fix.** Never automatically escalate from copy to adoption. Keep copy selected and explain insufficient capacity; adoption requires explicit confirmation of the canonical source. Store one immutable submitted intent, reset UI and state together on rescan, prevent rescan while submitting or track the existing operation separately, and abort polling on teardown.

**Implementation excerpt:**

```javascript
// At the start of rendering a NEW scan:
runtime = 'sim';
mode = 'copy';
setMode('copy'); // Do not set adopt merely because enough_space is false.
// Before POSTing the operation:
if (mode === 'copy' && !scan.enough_space) {
  toast('Not enough space to copy. Free space or explicitly choose adoption.', 'err');
  return;
}
if (mode === 'adopt' && !confirm(`Manage ${scan.path} in place? Stop its other manager first.`)) return;
const intent = Object.freeze({
  path: scan.path, mode, runtime,
  template: $('#impTemplate', out).value,
  name: $('#impName', out).value.trim(),
  port: Number($('#impPort', out).value),
});
// Disable Scan while submitting; poll by immutable operation ID and signal.
```

**Required regression verification.** Choose Docker, rescan, inspect both selected controls and submitted JSON. Test low disk, explicit adoption confirmation, rescan during a job and navigation away during an outstanding poll. No stale response may redirect a later route.

<a id="r62"></a>
### R62. File and configuration editors have no optimistic concurrency protection

**Priority:** Medium · **Evidence:** Confirmed lost-update risk  
**Source:** [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js); [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js); [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go)  
**Relevant code:** openEditor; WriteFile; settings saves; edit transaction  
**Relationship to prior work:** Additional data-integrity improvement.

**Problem and impact.** The editor reads a file and later replaces it without a revision/ETag precondition. Two administrators, or a panel edit and another writer, can silently overwrite newer data. Closing a dirty editor also lacks a reliable unsaved-change contract. A panel mutex only serializes writes as they arrive; it does not tell a late writer that its read was stale. An external game process requires an additional ownership/stopped-state policy beyond panel-side ETags.

**Suggested fix.** Return a strong content ETag with reads, require If-Match on replacements and compare under the edit transaction. Return 412 without discarding the client's draft. New-file creation uses an explicit no-replace precondition. Block or explicitly coordinate edits to game-owned live configuration; ETags alone cannot serialize uncooperative external writers.

**Implementation excerpt:**

```go
func contentETag(content []byte) string {
    sum := sha256.Sum256(content)
    return `"` + hex.EncodeToString(sum[:]) + `"`
}
// Under fs/edit gates, after rooted bounded reading of the CURRENT bytes:
if r.Header.Get("If-Match") == "" {
    http.Error(w, "If-Match required", http.StatusPreconditionRequired); return
}
if r.Header.Get("If-Match") != contentETag(currentBytes) {
    http.Error(w, "file changed; reload or merge your draft", http.StatusPreconditionFailed); return
}
// Publish via rooted atomic replacement, then return the new ETag.
// Frontend sends the read ETag and keeps the textarea open on 412.
// A dirty close/navigation asks for confirmation or preserves a local draft.
```

**Required regression verification.** Open the same file in two clients, save one, then save the other. The second must conflict with its draft intact. Test creation collisions, settings revisions and an external writer according to the declared live-edit policy.

<a id="r63"></a>
### R63. The file UI hides the fact that a directory listing was truncated

**Priority:** Low · **Evidence:** Confirmed backend/frontend contract gap  
**Source:** [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go); [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js)  
**Relevant code:** bounded ListFiles response; viewFiles rendering  
**Relationship to prior work:** Known-open A17 pagination; additional UI omission.

**Problem and impact.** The backend returns a bounded page and a truncation flag, but the file UI renders the page as though it were the complete directory and provides no way to reach the remainder. Large directories can therefore appear to be missing files. This is not an unbounded backend listing finding—the cap is already present and should be retained.

**Suggested fix.** Immediately display an explicit truncation warning. Add bounded stable pagination with a cursor bound to server/path/principal and a directory generation or bounded listing snapshot. Do not implement a fake cursor by repeatedly taking the same first page or sorting unrelated partial batches.

**Implementation excerpt:**

```javascript
// After rendering data.entries in viewFiles:
if (data.truncated) {
  const notice = document.createElement('div');
  notice.className = 'row';
  notice.textContent = 'This listing is incomplete: the directory exceeds the current page limit.';
  list.appendChild(notice);
}
// Once the API returns a validated next_cursor:
// Load more requests the same server/path with next_cursor and merges only
// that generation. Changed-generation responses ask the user to refresh.
```

**Required regression verification.** Create more entries than the cap and verify visible incompleteness. Pagination tests must reach every entry exactly once in a stable snapshot, handle changes explicitly and enforce cursor expiry/admission bounds.

<a id="r64"></a>
### R64. Malformed plugin SHA-256 input silently disables verification

**Priority:** High · **Evidence:** Confirmed fail-open integrity check; isolated proof  
**Source:** [`internal/arcade/plugins.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/plugins.go); [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)  
**Relevant code:** expected digest decoding and comparison during InstallPlugin  
**Relationship to prior work:** Incomplete A37 optional integrity verification.

**Problem and impact.** The optional expected digest is checked only when hex decoding succeeds and yields exactly 32 bytes. A supplied malformed value therefore behaves like no digest, rather than causing a validation error. An operator can believe a checksum was enforced when a typo made it inert. Existing full ZIP/CRC validation is valuable, but it proves structural integrity, not that the intended artifact was obtained. Proof 03 reproduces the fail-open condition.

**Suggested fix.** Distinguish omitted from invalid at request validation before starting the download. Use H01 `ExpectedSHA256` and compare a well-formed supplied digest against the bytes actually staged for publication. Show whether verification occurred in the result and expose the field in the UI.

**Implementation excerpt:**

```go
expected, err := ExpectedSHA256(requestedSHA256)
if err != nil { return err } // Fail before any network request.
hash := sha256.New()
// Download with the existing context and size budget into tempFile + hash.
_, err = io.Copy(io.MultiWriter(tempFile, hash), boundedResponseBody)
if err != nil { return err }
if expected != nil && subtle.ConstantTimeCompare(expected, hash.Sum(nil)) != 1 {
    return fmt.Errorf("plugin SHA-256 mismatch")
}
// Run ZIP/CRC validation, sync/close, revalidate cancellation and publish
// no-replace. Return digest_verified=true only when an expected digest matched.
```

**Required regression verification.** Test omitted, correct, wrong, odd-length, short, nonhex, whitespace and uppercase digests. Malformed input must never initiate a download; mismatches must never publish a jar. Preserve existing collision and cancellation tests.

<a id="r65"></a>
### R65. Plugin download transport and network access need an explicit trust policy

**Priority:** Medium · **Evidence:** Confirmed hardening gap; not asserted as an unconditional privilege escalation  
**Source:** [`internal/arcade/plugins.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/plugins.go); [`cmd/teploy-arcade/frontend/views-plugins.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-plugins.js)  
**Relevant code:** download URL policy; redirects; publication  
**Relationship to prior work:** Known-open A37 HTTPS policy; broader network-policy hardening.

**Problem and impact.** HTTP is allowed for executable plugin downloads, and server-side fetching has a broad network reach unless constrained by deployment. This increases substitution and internal-network-request risk. Whether private mirrors should be allowed is a product/deployment decision; an authenticated operator's existing authority and game-network access matter to the threat model. A checksum is useful only when supplied from an independently trusted source; a valid jar can still be malicious code.

**Suggested fix.** Default to HTTPS, validate every redirect, expose any insecure/private-mirror exception as an explicit audited policy, and apply download/validation concurrency budgets. If restricting destinations, enforce policy on the actual dialed IP as well as URL names to prevent DNS-rebinding gaps. Keep plugin execution trust clearly separate from ZIP validity.

**Implementation excerpt:**

```go
func allowedPluginURL(u *url.URL) error {
    if u.Scheme != "https" { return fmt.Errorf("plugin downloads require HTTPS") }
    if u.Hostname() == "" || u.User != nil { return fmt.Errorf("invalid plugin URL") }
    if u.Fragment != "" { return fmt.Errorf("plugin URL fragments are unsupported") }
    return nil
}
client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
    if len(via) >= 5 { return fmt.Errorf("too many redirects") }
    return allowedPluginURL(req.URL)
}
// Validate the initial URL too. A public-only deployment additionally uses a
// policy-aware DialContext that validates AND connects to the resolved IP,
// including IPv4-mapped IPv6, without a second uncontrolled DNS resolution.
```

**Required regression verification.** Test HTTPS→HTTP downgrade redirects, credentials in URLs, private/link-local/loopback destinations under each configured policy, DNS changes between validation and dialing, oversized validation work and simultaneous installs.

<a id="r66"></a>
### R66. Several small UI affordances claim unavailable behavior or ignore cancellation

**Priority:** Low · **Evidence:** Confirmed user-facing defects  
**Source:** [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js); [`cmd/teploy-arcade/frontend/views-plugins.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-plugins.js); [`cmd/teploy-arcade/frontend/views-import.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-import.js)  
**Relevant code:** backup prompt; disk free display; plugin upload/help/install form  
**Relationship to prior work:** New UI oversights; checksum backend issue tracked in R64.

**Problem and impact.** Canceling the optional backup-note prompt still creates a backup because null is converted to an empty string and execution continues (proof 16). Zero free bytes is displayed as unknown through a truthiness check. The plugin view advertises binary upload through Files although that interface only edits text and has no jar-upload workflow, and it provides no checksum input for the backend feature. Enter-key submission paths also need a busy guard, not merely a disabled click button. Some unclaimed-panel explanatory text still describes the old open behavior.

**Suggested fix.** Honor cancellation, distinguish zero from unknown, remove inaccurate instructions until upload exists, add the optional SHA-256 field and result state, and use one submission-in-flight flag across click/Enter. Update first-run text to describe the setup gate rather than unauthenticated administrative access.

**Implementation excerpt:**

```javascript
const note = prompt('Note for this backup (optional)');
if (note === null) return;
// Zero is a real measurement; null/undefined is unknown.
$('#bkFree', root).textContent = data.free_bytes == null
  ? 'unknown' : humanBytes(data.free_bytes);
// Use this at the top of BOTH click and Enter install handling:
if (installing) return;
installing = true;
try {
  await api(`/api/servers/${id}/plugins/install`, {
    method: 'POST', body: JSON.stringify({ url, sha256: shaInput.value.trim() }),
  });
} finally { installing = false; }
// Add shaInput to the form and replace the nonexistent-upload help text with
// an honest supported workflow. Wire the added sha256 field to the API request.
```

**Required regression verification.** Cancel the note prompt and assert no network mutation. Show zero free bytes, double-submit by Enter/click, validate checksum feedback, and verify every advertised UI workflow actually exists.

<a id="r67"></a>
### R67. Keyboard interaction, modal focus and the skip link are incomplete

**Priority:** Low · **Evidence:** Confirmed markup/interaction gaps; browser audit still required  
**Source:** [`cmd/teploy-arcade/frontend/index.html`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/index.html); [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js); [`cmd/teploy-arcade/frontend/views2.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views2.js); [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js)  
**Relevant code:** skip-link href; custom toggle/tab elements; modal focus  
**Relationship to prior work:** Accessibility improvement beyond routing/syntax tests.

**Problem and impact.** Several clickable spans/divs/custom checkbox labels lack native keyboard semantics. Custom modal overlays need focus containment/restoration and accessible names. The skip link's `#view` target also participates in the SPA hash router and can trigger a route change instead of only moving focus into the current content. The HTML/CSS source supports these findings; no full assistive-technology/browser audit was run.

**Suggested fix.** Prefer native buttons/checkboxes and a dialog element, provide accessible labels/state, and manage focus on route changes. Make the skip link move focus without changing the routing hash. Retain visible focus and test the complete keyboard workflow, not only ARIA attributes.

**Implementation excerpt:**

```javascript
const main = document.querySelector('#view');
main.tabIndex = -1;
document.querySelector('.skip-link').addEventListener('click', event => {
  event.preventDefault();
  main.focus(); // Preserve the current route hash.
});
// Modal pattern:
const opener = document.activeElement;
const dialog = document.createElement('dialog');
dialog.setAttribute('aria-label', 'Edit file');
dialog.addEventListener('close', () => { dialog.remove(); opener?.focus(); });
// Populate with real buttons/labels; append, then dialog.showModal().
// Scheduler toggles become <input type="checkbox"> or an accessible button
// with aria-pressed, not a mouse-only span.
```

**Required regression verification.** Keyboard-only tests for skip link, navigation, task toggles, modal open/Escape/close and focus return. Assert the skip link preserves the current route and screen-reader labels describe controls and validation errors.

<a id="r68"></a>
### R68. The advertised game address is derived from a listener address and is not IPv6-safe everywhere

**Priority:** Low · **Evidence:** Confirmed address-presentation/deployment weakness  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js)  
**Relevant code:** Host response; server join address formatting  
**Relationship to prior work:** Residual presentation issue beyond A33 IPv6 listener fix.

**Problem and impact.** A panel bind address such as 0.0.0.0, :: or localhost is not necessarily a usable player connection address. Client concatenation of host + colon + port also fails for raw IPv6 literals. The earlier JoinHostPort fix for the panel listener/banner does not automatically fix every game-address display. Reverse-proxy hostnames can differ from the game host too.

**Suggested fix.** Configure an explicit public game host/address policy and format addresses with an IPv6-aware helper. Mark unavailable/locally reachable addresses as such; do not silently trust arbitrary forwarded Host headers as a public game endpoint.

**Implementation excerpt:**

```go
func gameJoinAddress(publicHost string, port int) (string, error) {
    if publicHost == "" || strings.ContainsAny(publicHost, "\r\n/ ") {
        return "", fmt.Errorf("configure a public game host")
    }
    if port < 1 || port > 65535 { return "", fmt.Errorf("invalid game port") }
    if ip := net.ParseIP(publicHost); ip != nil && ip.IsUnspecified() {
        return "", fmt.Errorf("wildcard listener is not a public game address")
    }
    return net.JoinHostPort(publicHost, strconv.Itoa(port)), nil
}
// Store the configured host without IPv6 brackets; return the formatted
// address in the API instead of rebuilding it inconsistently in each view.
```

**Required regression verification.** Test DNS name, IPv4, IPv6 literal, wildcard and loopback-only deployment. A reverse-proxied panel must be able to advertise a different configured game hostname.

## Build, deployment and application lifecycle

<a id="r69"></a>
### R69. The release metadata expression rejects every image publication

**Priority:** High · **Evidence:** Confirmed against the action’s actual parser; isolated predicate proof  
**Source:** [`.github/workflows/release.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.github/workflows/release.yml)  
**Relevant code:** docker/metadata-action tags: enable=!is_prerelease  
**Relationship to prior work:** Incomplete/incorrect A39 release change.

**Problem and impact.** The workflow passes the literal `!is_prerelease` as the enable attribute of a raw latest tag. docker/metadata-action@v5 expands supported expressions and then requires the result to be exactly true or false; it does not interpret that bare text as a boolean negation. The reviewed action source therefore throws before producing tags. This affects stable as well as prerelease image publication. The comments inside the tag block are supported by the action and are not the problem. Proof 15 exercises the rejecting predicate, not an actual GitHub Actions run.

**Suggested fix.** Remove the invalid raw latest rule and use metadata-action's semver-driven automatic latest policy. Keep the verify dependency already added. Test real action inputs for stable/prerelease tags before relying on a release workflow as validation of itself.

**Implementation excerpt:**

```yaml
- uses: docker/metadata-action@v5
  id: meta
  with:
    images: ghcr.io/useteploy/teploy-arcade
    flavor: |
      latest=auto
    tags: |
      type=semver,pattern={{version}}
      type=semver,pattern={{major}}.{{minor}}
# Remove: type=raw,value=latest,enable=!is_prerelease
# Pin the action to a reviewed immutable commit as part of R71.
# Stable semver tags receive latest automatically; prereleases do not.
```

**Required regression verification.** Run the actual action for v1.2.3, v1.2.3-rc.1 and invalid/non-semver tags in a non-publishing validation job. Assert no parser error, expected image tags, and no prerelease update to latest.

**Primary external reference:** [metadata-action v5 enable parser](https://github.com/docker/metadata-action/blob/v5/src/meta.ts), [tag parser](https://github.com/docker/metadata-action/blob/v5/src/tag.ts), and [input/comment handling](https://github.com/docker/metadata-action/blob/v5/src/context.ts). The external action was read through GitHub, but its v5 tag is mutable.

<a id="r70"></a>
### R70. The Alpine runtime base is outside normal scheduled support

**Priority:** Medium · **Evidence:** Verified current maintenance risk as of 2026-09-19  
**Source:** [`Dockerfile`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/Dockerfile)  
**Relevant code:** runtime stage FROM alpine:3.20  
**Relationship to prior work:** Known-open supply-chain program, with a concrete support-date finding.

**Problem and impact.** The runtime stage uses Alpine 3.20. Alpine's official release table lists its scheduled support end as April 1, 2026 and subsequent support as on request. That is past the audit date. This is a maintenance/security-update risk, not proof of a specific installed-package CVE. The actual released image's package inventory and vulnerabilities were not scanned.

**Suggested fix.** Move to a normally supported base after compatibility testing, pin a reviewed digest and automate reviewed updates. At this audit date Alpine 3.24 is a supported release branch. Rebuild and scan the resulting image, including Docker CLI/CA certificates, rather than assuming a tag change alone certifies security.

**Implementation excerpt:**

```dockerfile
# Localized runtime-stage upgrade, followed by digest pinning after review:
FROM alpine:3.24
RUN apk add --no-cache docker-cli ca-certificates tzdata
# Retain the existing binary copy, data setup and entrypoint as appropriate.
# Test CLI/daemon compatibility and supported architectures before promotion.
# A digest must be resolved from the reviewed build; do not invent a hash.
```

**Required regression verification.** Build and smoke-test the image, verify Docker connectivity/TLS/timezone behavior, generate an SBOM and scan the exact digest. Establish a scheduled base-image update/rebuild check.

**Primary external reference:** [Alpine official support table](https://www.alpinelinux.org/releases/), consulted September 19, 2026.

<a id="r71"></a>
### R71. Release reproducibility and verification still leave supply-chain/test gaps

**Priority:** Medium · **Evidence:** Confirmed hardening and validation gaps  
**Source:** [`.github/workflows/ci.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.github/workflows/ci.yml); [`.github/workflows/release.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.github/workflows/release.yml); [`.goreleaser.yaml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.goreleaser.yaml); [`Dockerfile`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/Dockerfile); [`go.mod`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/go.mod)  
**Relevant code:** floating actions/tool/base/game versions; goreleaser before hooks; test coverage  
**Relationship to prior work:** Known-open A40/A34 browser testing; independently corroborated.

**Problem and impact.** Actions use mutable major tags, GoReleaser is latest, base/game images float, and the release before hook runs go mod tidy after the verified commit. The existing CI/race/routing checks and release verify dependency are real improvements, but they do not validate runtime image contracts, browser interactions, power-loss recovery, workflow metadata semantics or the exact released dependency/image inventory. No vulnerability scan was run during this audit, and no unrelated WebSocket advisory is being attributed to this repository.

**Suggested fix.** Pin reviewed action commits/tool versions/image digests with update automation. Remove release-time dependency mutation; require a clean dependency manifest. Add govulncheck, exact-image scanning, browser interaction tests, crash fault injection and per-template Docker contract tests. Treat vulnerability findings as scan outputs to triage, not infer them from version numbers alone.

**Implementation excerpt:**

```yaml
# Replace the mutating GoReleaser hook with validation:
before:
  hooks:
    - go mod verify
    - git diff --exit-code -- go.mod go.sum
# CI additions (shell step; configure a REVIEWED exact tool version):
# : "${GOVULNCHECK_VERSION:?pin a reviewed govulncheck version}"
# go run golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION} ./...
# go test -race ./...
# node --test
# Browser, crash-recovery and Docker contract suites are separate required jobs.
# Add dependency/update automation for gomod, github-actions and image digests.
```

**Required regression verification.** Prove the release tree stays unchanged after build, validate metadata inputs without publishing, scan the produced digest and run browser/runtime/crash suites on the same candidate commit. Retain existing checks rather than replacing them with the new ones.

**Primary external reference:** [Go vulnerability tooling](https://go.dev/doc/security/vuln/).

<a id="r72"></a>
### R72. Run starts unowned workers before binding and does not provide a complete shutdown contract

**Priority:** Medium · **Evidence:** Confirmed application lifecycle gap  
**Source:** [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go); [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/metrics.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/metrics.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** Run worker startup; background loops; globals and shutdown  
**Relationship to prior work:** Known-open pre-existing worker item/A33.

**Problem and impact.** Long-lived workers start before the HTTP bind is known to succeed and do not all share a caller-visible cancellation/join contract. A bind failure or embedding/reusing Run can leave work behind. Package-global configuration/catalog state also complicates multiple instances and tests. Catching a worker panic and only logging it can leave a critical service silently dead. Game-container survival across panel restarts is intentional and must be separated from panel-owned watcher lifetime.

**Suggested fix.** Accept an application context, bind first, start context-aware workers only after successful initialization, and stop/join them on every exit. Keep configuration on the application/manager instance. Surface critical worker failures as health failures or terminate/restart deliberately. Shutdown cancels panel watchers/jobs according to policy, not automatically every game container.

**Implementation excerpt:**

```go
func serveOwned(ctx context.Context, ln net.Listener, server *http.Server,
    workers ...func(context.Context)) error {
    workCtx, cancel := context.WithCancel(ctx)
    var wg sync.WaitGroup
    defer func() { cancel(); wg.Wait() }()
    for _, worker := range workers {
        wg.Add(1)
        go func(f func(context.Context)) { defer wg.Done(); f(workCtx) }(worker)
    }
    result := make(chan error, 1)
    go func() { result <- server.Serve(ln) }()
    select {
    case err := <-result:
        if errors.Is(err, http.ErrServerClosed) { return nil }; return err
    case <-ctx.Done():
        shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
        defer stop()
        if err := server.Shutdown(shutdownCtx); err != nil { _ = server.Close(); return err }
        return nil
    }
}
// Construct ln with net.Listen BEFORE calling this function. Every worker
// must select on ctx.Done; a worker ignoring cancellation still cannot be joined.
```

**Required regression verification.** Force bind failure, cancel while idle/busy, and run two separately configured instances in a test process. Assert no panel-owned goroutines/listeners leak, critical worker failures are visible and the chosen game-container continuity policy is preserved.

<a id="r73"></a>
### R73. Corrupt durable state needs an explicit recovery mode, not silent replacement of the active fleet

**Priority:** Medium · **Evidence:** Confirmed recovery-policy weakness  
**Source:** [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go); [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go); [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)  
**Relevant code:** Load quarantine/fallback; record validation; startup sequencing  
**Relationship to prior work:** Recovery policy beyond A09 null-record fixes.

**Problem and impact.** Null/duplicate-record protections and setup gating already improve safety. However quarantine/drop/fallback behavior can leave the panel with a newly seeded or incomplete registry while real containers/data remain. Operators need to distinguish a truly fresh installation from a corrupt previously active store. A syntactically valid users store with no administrator also needs an explicit local recovery policy. It must not silently reopen authorization; the current setup gate should be retained.

**Suggested fix.** Make critical-store corruption enter an authenticated/local recovery mode and preserve the original bytes. Validate semantic record invariants before publishing any registry. Seed demos only through explicit fresh-install/demo intent, not as a generic error fallback. Recover journals and reconcile an identified fleet before automatic startup.

**Implementation excerpt:**

```go
func loadRequiredState(statePath string, dst any) (fresh bool, err error) {
    root, err := os.OpenRoot(filepath.Dir(statePath))
    if err != nil { return false, fmt.Errorf("state directory unavailable: %w", err) }
    defer root.Close()
    b, err := readRegularRooted(root, filepath.Base(statePath), 32<<20)
    if errors.Is(err, os.ErrNotExist) { return true, nil }
    if err != nil { return false, fmt.Errorf("state unavailable: %w", err) }
    if err := json.Unmarshal(b, dst); err != nil {
        return false, fmt.Errorf("state corrupt; preserve file and enter recovery: %w", err)
    }
    return false, nil
}
// Private directory is provisioned before Load; use the per-store byte cap.
// Validate semantic identities/roles/runtime/geometry before publishing.
// Only fresh==true plus explicit policy may seed demonstration data.
```

**Required regression verification.** Test missing vs unreadable vs truncated vs semantically invalid state, an existing fleet with corrupt registry, and a valid user set without admins. No implicit demo creation, destructive reconciliation or unauthenticated operational access is allowed.

<a id="r74"></a>
### R74. CLI default-path evaluation has side effects before flags determine whether storage is needed

**Priority:** Low · **Evidence:** Confirmed initialization-order oversight  
**Source:** [`cmd/teploy-arcade/main.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/main.go)  
**Relevant code:** defaultDataDir invocation during flag initialization  
**Relationship to prior work:** Additional CLI usability/initialization improvement.

**Problem and impact.** The default data-directory selection is evaluated while defining/parsing flags, before the program knows it only needs help/version output or an explicit custom data path. Directory probing/creation at that point is unnecessary and can select or touch an unexpected location. Startup should also report an existing-but-unusable data directory rather than quietly treating existence as suitability.

**Suggested fix.** Parse flags without resolving the default, return for help/version, then resolve and validate the data directory only for a real application start. Keep all filesystem initialization after configuration validation and before worker creation.

**Implementation excerpt:**

```go
var data string
var showVersion bool
flag.StringVar(&data, "data", "", "data directory (default resolved at startup)")
flag.BoolVar(&showVersion, "version", false, "print version")
flag.Parse()
if showVersion { fmt.Println(version); return }
if data == "" { data = defaultDataDir() }
// Validate absolute/canonical path and actual read/write/create permissions,
// then initialize the application. --help/--version must not create storage.
// An explicit -data value must not probe/create the unrelated default tree.
```

**Required regression verification.** Run --help/--version/custom -data with an unwritable home/default location and monitor filesystem changes. No unrelated directory should be created or selected. Verify clear errors for unusable explicit storage.

<a id="repair-sequence"></a>
## Repair sequence and acceptance gates

| Wave | Scope | Acceptance gate |
|---|---|---|
| 1 — contain direct exposure and prevent destructive uncertainty | R01–R03, R13, R18/R20, R35, R47, R64, R69; disable unverified native templates pending R48 | No wildcard no-auth; setup transition stays gated; no ownership escape; unknown recovery/Docker state refuses destructive effects; no default public RCON; malformed checksum rejects; release metadata fixtures pass. |
| 2 — make state and recovery coherent | R16–R25, R27–R34, R37/R38, R42, R50, R62/R73 | Fault-injected commits return an honest outcome; tree/model/bindings agree after repeatable recovery; deletion retains recoverability; no lost edits; all long jobs have owned resources and durable status. |
| 3 — unify admission and lifecycle | R04–R12, R30/R34/R36, R39–R46, R51–R56, R65/R72 | Bounded requests/connections/jobs/storage; typed capabilities; scoped accurate metrics; cancellation/occurrence semantics; real-image contract tests; no worker leaks. |
| 4 — close interaction and maintenance gaps | R57–R63, R66–R68, R70/R71/R74 | Browser/keyboard tests, honest pagination/address/status, no stale route or unintended adoption, supported and scanned release base, reproducible release process. |

Do not independently land a new strict decoder before updating clients that send stale full task objects. Do not remove raw MCP tools without a deliberate token/client migration. Do not change Rust/Valheim mount paths by recreating existing containers before recovering their current world data. Do not replace a generic state writer with a post-rename-error-returning writer until callers handle the Published outcome correctly.

For concurrency changes, document and enforce the actual lock graph. Keep filesystem admission outside lifecycle claims as required by current code; never acquire Save's server locks while already holding s.mu. Long external I/O must not hold manager/auth/global lifecycle mutexes. Copy immutable snapshots before releasing locks. Every new gate must cover HTTP, WebSocket, MCP, scheduler and simulator paths, not only one handler.

## Verification plan for the real repository

Run these only in a disposable development checkout at the pinned commit, with the declared Go 1.26 toolchain or a validated supported compatible version. They were **not run as repository checks in this audit**.

```sh
git clone https://github.com/useteploy/teploy-arcade.git
cd teploy-arcade
git checkout --detach 329e67e73352aa7f972182ee02bd489d73234d8c
go version
go mod verify
gofmt -l cmd internal
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./cmd/teploy-arcade
# Run the repository's checked-in Node test entry points as specified in CI.
# Then run the NEW per-finding tests; a clean existing suite is not closure.
```

Use a fake Docker executable/Engine test server for deterministic timeout/unknown/identity tests. Use subprocess crash injection and a disposable filesystem for every restore/delete journal phase. Use a real browser for route ordering, authentication expiry, role affordances, dirty editors and accessibility. Use pinned disposable game images for persistence, RCON, port geometry, readiness and live-save acknowledgment tests. Never use a production world or public service as an exploit/test target.

Full closure requires a source diff, a failing-before/passing-after regression for the concrete defect, and successful targeted integration tests. Larger transaction/runtime changes additionally require crash/restart fixtures and documented migration behavior. Review scans of the exact release image and dependency tree; this report does not certify their vulnerability status.

## Shared reference implementations

## H01. Tested standalone safety helpers

The following complete file was compiled and exercised by six isolated tests under Go 1.23.2. It is placed in package `auditproofs` for standalone reproduction; integration into the repository changes the package/import organization and call sites. Tests cover the named normal/boundary cases, **not** every failure injection or filesystem durability guarantee. `AtomicStateWrite` is intentionally limited to a private, panel-owned state directory and must not replace rooted game-tree I/O.

<!-- file: reference_fixes.go -->
```go
package auditproofs

// Standalone reference implementations, NOT an integrated repository patch.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// CommitOutcome distinguishes a failed preparation from an already visible
// replacement whose durability could not be confirmed. Callers must NOT
// revert memory to the old value when Published is true.
type CommitOutcome struct{ Published, Durable bool }

// AtomicStateWrite is for files in a PRIVATE, PANEL-OWNED state directory.
// It is not a replacement for os.Root-based writes to a mutable game tree.
// The directory must exist and be durably provisioned before calling it.
func AtomicStateWrite(path string, data []byte, mode os.FileMode) (out CommitOutcome, err error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return out, err
	}
	defer dir.Close()
	st, err := dir.Stat()
	if err != nil {
		return out, err
	}
	if !st.IsDir() {
		return out, fmt.Errorf("state parent is not a directory")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".arcade-tmp-state-*")
	if err != nil {
		return out, err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tmp)
	}()
	if err = f.Chmod(mode.Perm()); err != nil {
		return out, err
	}
	if _, err = f.Write(data); err != nil {
		return out, err
	}
	if err = f.Sync(); err != nil {
		return out, err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return out, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return out, err
	}
	out.Published = true
	if err = dir.Sync(); err != nil {
		return out, fmt.Errorf("replacement visible; directory sync failed: %w", err)
	}
	out.Durable = true
	return out, nil
}

func ExpectedSHA256(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	} // Optional really means absent, not invalid.
	if len(text) != 2*sha256.Size {
		return nil, fmt.Errorf("sha256 must contain exactly 64 hexadecimal characters")
	}
	b, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("invalid sha256: %w", err)
	}
	return b, nil
}

func CredentialBounds(name, password string) error {
	if !utf8.ValidString(name) || !utf8.ValidString(password) {
		return fmt.Errorf("credentials must be valid UTF-8")
	}
	if len(name) == 0 || len(name) > 128 {
		return fmt.Errorf("username must be 1..128 UTF-8 bytes")
	}
	if len(password) < 8 || len(password) > 1024 {
		return fmt.Errorf("password must be 8..1024 UTF-8 bytes")
	}
	return nil
}

func MergePending(previous, added []string) []string {
	seen := make(map[string]struct{}, len(previous)+len(added))
	for _, group := range [][]string{previous, added} {
		for _, key := range group {
			if key != "" {
				seen[key] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

type HTTPInputError struct {
	Status  int
	Message string
}

func (e *HTTPInputError) Error() string { return e.Message }

// DecodeJSON is for JSON-bearing routes, NOT bodiless lifecycle actions.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("invalid JSON body limit")
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &HTTPInputError{http.StatusUnsupportedMediaType, "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	classify := func(err error) error {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &HTTPInputError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &HTTPInputError{http.StatusBadRequest, "invalid JSON request: " + err.Error()}
	}
	if err := d.Decode(dst); err != nil {
		return classify(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return &HTTPInputError{http.StatusBadRequest, "exactly one JSON value is required"}
		}
		return classify(err)
	}
	return nil
}

type cappedOutput struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := b.limit - b.buf.Len()
	if len(p) > left {
		b.exceeded = true
		p = p[:left]
	}
	_, _ = b.buf.Write(p)
	// Drain discarded bytes so a full pipe cannot hang the child. The
	// operation deadline separately bounds total process lifetime.
	return n, nil
}

func BoundedCommand(parent context.Context, timeout time.Duration, outputLimit int, program string, args ...string) ([]byte, error) {
	if timeout <= 0 || outputLimit <= 0 {
		return nil, fmt.Errorf("positive command limits required")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = 2 * time.Second
	out := &cappedOutput{limit: outputLimit}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.exceeded {
		err = errors.Join(err, fmt.Errorf("command output exceeded %d bytes", outputLimit))
	}
	return append([]byte(nil), out.buf.Bytes()...), err
}

func WaitContext(ctx context.Context, duration time.Duration) error {
	if duration < 0 {
		return fmt.Errorf("negative wait")
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
```

## H02. Root-confined regular-file and directory I/O (Go 1.26 integration)

These functions target the repository's Linux/Darwin platforms and its declared Go toolchain. They were **not compiled in this environment**, whose Go version is 1.23.2. `os.Root` and its `Lchown` method are available before Go 1.26; the primary API documentation was checked. Keep platform-specific flags behind the repository's existing platform conventions.

A held root prevents path traversal outside that root. It does not freeze file contents, isolate hard-linked inodes, stop an external process from writing, or guarantee that a root initially opened from an untrusted absolute pathname is the intended directory. Pin/validate the server-root identity at operation admission. Stop or coordinate the game when consistency, rather than merely confinement, is required.

```go
// Imports: fmt, io, io/fs, os, path/filepath, syscall.
func openRegularRooted(root *os.Root, name string) (*os.File, os.FileInfo, error) {
    f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
    if err != nil { return nil, nil, err }
    info, err := f.Stat()
    if err != nil {
        _ = f.Close()
        return nil, nil, err
    }
    if !info.Mode().IsRegular() {
        _ = f.Close()
        return nil, nil, fmt.Errorf("%q is not a regular file", name)
    }
    return f, info, nil
}

func readRegularRooted(root *os.Root, name string, limit int64) ([]byte, error) {
    if limit < 0 || limit > 1<<30 { return nil, fmt.Errorf("invalid read limit") }
    f, info, err := openRegularRooted(root, name)
    if err != nil { return nil, err }
    defer f.Close()
    if info.Size() > limit { return nil, fmt.Errorf("%q exceeds size limit", name) }
    b, err := io.ReadAll(io.LimitReader(f, limit+1))
    if err != nil { return nil, err }
    if int64(len(b)) > limit { return nil, fmt.Errorf("%q grew beyond size limit", name) }
    return b, nil
}

func openDirectoryRooted(root *os.Root, name string) (*os.File, error) {
    f, err := root.OpenFile(name,
        os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
    if err != nil { return nil, err }
    info, err := f.Stat()
    if err != nil || !info.IsDir() {
        _ = f.Close()
        if err != nil { return nil, err }
        return nil, fmt.Errorf("%q is not a directory", name)
    }
    return f, nil
}

// Rooted, bounded traversal. Each child's actual open must still validate its
// descriptor; DirEntry metadata is not an authorization/type guarantee.
func walkRooted(root *os.Root, start string, maxEntries int,
    visit func(string, fs.FileInfo) error) error {
    if maxEntries <= 0 || visit == nil { return fmt.Errorf("invalid walk budget") }
    count := 0
    var walk func(string, int) error
    walk = func(name string, depth int) error {
        if depth > 64 { return fmt.Errorf("directory depth limit exceeded") }
        count++
        if count > maxEntries { return fmt.Errorf("entry limit exceeded") }
        info, err := root.Lstat(name)
        if err != nil { return err }
        if err := visit(name, info); err != nil { return err }
        if !info.IsDir() { return nil } // Never traverse a final symlink.
        dir, err := openDirectoryRooted(root, name)
        if err != nil { return err }
        defer dir.Close()
        for {
            entries, err := dir.ReadDir(128)
            for _, entry := range entries {
                if err := walk(filepath.Join(name, entry.Name()), depth+1); err != nil { return err }
            }
            if err == io.EOF { return nil }
            if err != nil { return err }
        }
    }
    return walk(start, 0)
}
```

For ownership repair, call `root.Lchown` on entries of the **newly extracted tree only**. If existing files can have external hard links, restrict ownership repair to newly created descriptors or reject unsupported hard-linked content; root confinement is not an inode-ownership sandbox. For archives/copies, call `openRegularRooted` and derive the tar header/copy length from its `FileInfo`, not the walk's earlier metadata. For player/config reads, apply per-format byte and entry-count limits.

## H03. Conservative properties safety mode

This is an immediately implementable **restricted-format alternative**, not a claim to implement the complete Java Properties language. It rejects otherwise valid escaped/continued/colon-separated/duplicate-key files rather than silently misinterpreting them. That compatibility cost must be explicit in the UI. A full parser/serializer should eventually replace it and be differential-tested against the Java loader actually used by the supported game.

Apply this to Minecraft's root managed properties only—not an unrelated plugin's similarly named file, not a proxy's TOML/YAML, and not a non-Minecraft game's configuration. Use the same parser for file editing, import, restore, model reload and binding admission. The raw file write and resulting model update still need R23's transaction.

```go
// Imports: bufio, fmt, regexp, strconv, strings, unicode/utf8.
var simplePropertyKey = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func parseRestrictedProperties(text string, requirePort bool) (map[string]string, error) {
    if len(text) > 2<<20 || !utf8.ValidString(text) {
        return nil, fmt.Errorf("properties must be bounded valid UTF-8")
    }
    text = strings.ReplaceAll(text, "\r\n", "\n")
    if strings.ContainsAny(text, "\r\x00") { return nil, fmt.Errorf("unsupported control character") }
    values := make(map[string]string)
    scanner := bufio.NewScanner(strings.NewReader(text))
    scanner.Buffer(make([]byte, 4096), 2<<20)
    lineNumber := 0
    for scanner.Scan() {
        lineNumber++
        line := strings.TrimLeft(scanner.Text(), " \t\f")
        if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") { continue }
        if strings.Contains(line, `\`) {
            return nil, fmt.Errorf("line %d: escaped/continued syntax requires the full properties parser", lineNumber)
        }
        key, value, ok := strings.Cut(line, "=")
        key = strings.TrimSpace(key)
        if !ok || !simplePropertyKey.MatchString(key) {
            return nil, fmt.Errorf("line %d: only unescaped key=value syntax is supported", lineNumber)
        }
        if _, exists := values[key]; exists {
            return nil, fmt.Errorf("line %d: duplicate property %q", lineNumber, key)
        }
        value = strings.TrimLeft(value, " \t\f")
        if strings.ContainsAny(value, "\x00\r\n") { return nil, fmt.Errorf("invalid value for %s", key) }
        values[key] = value
    }
    if err := scanner.Err(); err != nil { return nil, err }
    rawPort, hasPort := values["server-port"]
    if requirePort && !hasPort { return nil, fmt.Errorf("server-port must be explicit") }
    if hasPort {
        port, err := strconv.Atoi(rawPort)
        if err != nil || port < 1 || port > 65535 {
            return nil, fmt.Errorf("invalid final server-port")
        }
    }
    return values, nil
}

type SettingRule struct {
    Kind string
    Options []string
    Min, Max *int
}
func validateSetting(rule SettingRule, value string) error {
    if strings.ContainsAny(value, "\x00\r\n") { return fmt.Errorf("control characters are not allowed") }
    switch rule.Kind {
    case "bool":
        if value != "true" && value != "false" { return fmt.Errorf("expected true or false") }
    case "int":
        n, err := strconv.Atoi(value)
        if err != nil { return fmt.Errorf("expected integer") }
        if rule.Min != nil && n < *rule.Min { return fmt.Errorf("below allowed minimum") }
        if rule.Max != nil && n > *rule.Max { return fmt.Errorf("above allowed maximum") }
    case "enum":
        for _, option := range rule.Options { if value == option { return nil } }
        return fmt.Errorf("unsupported value")
    case "string":
        // Apply this property's byte bound and serialization policy separately.
    default:
        return fmt.Errorf("unsupported setting kind")
    }
    return nil
}
```

The full implementation must preserve comments/unknown keys where promised, or say clearly that canonicalization rewrites formatting. Do not serialize raw `value` strings containing line breaks; do not “repair” invalid managed identity by retaining the previous port only in memory. Validate the effective port span and all dynamic bindings before publishing.

## H04. Restore transaction and conservative recovery implementation

**Integration implementation, not a drop-in complete restore replacement.** The current repository does not have the proposed journal/operation model. The following implements the critical journal validation/publication and tree rollback mechanics; the extraction, ordered commit driver, metadata serializer and operation store must be wired into it. Do not deploy only the rollback helper and claim crash consistency.

Use `RestoreJournal`/`RestorePhase` from R19, plus a `rolled_back` terminal phase if desired. Store the journal in a **private panel-owned operation directory**, not in a game-writable location; bind it to a unique operation ID and the recorded server-root identity. Hold exclusive filesystem/lifecycle admission, confirm Docker is stopped rather than unknown, and reject outside writers/adopted-source activity while operating. The original/replacement name inventories must be complete and immutable before moving any entry.

```go
// Imports: encoding/json, errors, fmt, os, path/filepath, strings.
func validTopLevelName(name string) bool {
    return name != "" && name != "." && name != ".." &&
        filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`) &&
        !strings.HasPrefix(name, ".arcade-")
}

func validateRestoreJournal(j RestoreJournal) error {
    if j.Version != 1 || j.ServerID == "" { return fmt.Errorf("invalid restore journal identity") }
    switch j.Phase {
    case RestorePrepared, RestoreInstalling, RestoreCommitted:
    default: return fmt.Errorf("unknown restore phase")
    }
    for _, names := range [][]string{j.OriginalNames, j.ReplacementNames} {
        seen := make(map[string]bool, len(names))
        for _, name := range names {
            if !validTopLevelName(name) || seen[name] { return fmt.Errorf("invalid/duplicate journal entry %q", name) }
            seen[name] = true
        }
    }
    if !json.Valid(j.OldModel) || !json.Valid(j.NewModel) { return fmt.Errorf("invalid model snapshots") }
    return nil
}

func persistRestoreJournal(privatePath string, j RestoreJournal) (CommitOutcome, error) {
    if err := validateRestoreJournal(j); err != nil { return CommitOutcome{}, err }
    b, err := json.Marshal(j)
    if err != nil { return CommitOutcome{}, err }
    return AtomicStateWrite(privatePath, b, 0o600)
}

func syncRootDirectory(root *os.Root, name string) error {
    f, err := openDirectoryRooted(root, name)
    if err != nil { return err }
    syncErr := f.Sync()
    closeErr := f.Close()
    return errors.Join(syncErr, closeErr)
}

// Roll back a prepared/installing tree. This does NOT clear the recovery
// interlock or delete staging. Its caller must restore OldModel durably,
// record rollback completion, and only then clean up.
func rollbackRestoreTree(root *os.Root, staging string, j RestoreJournal) error {
    if err := validateRestoreJournal(j); err != nil { return err }
    if j.Phase == RestoreCommitted { return fmt.Errorf("committed restore must not be rolled back implicitly") }
    // staging is an operation-owned top-level directory; never accept request
    // input or an arbitrary directory discovered only by its prefix.
    if filepath.Base(staging) != staging || !strings.HasPrefix(staging, ".arcade-restore-") {
        return fmt.Errorf("invalid staging identity")
    }
    held := filepath.Join(staging, "old")
    original := make(map[string]bool, len(j.OriginalNames))
    for _, name := range j.OriginalNames { original[name] = true }

    // Entries unique to the replacement may have been installed only after
    // the INSTALLING journal phase was durably published.
    if j.Phase == RestoreInstalling {
        for _, name := range j.ReplacementNames {
            if original[name] { continue }
            if err := root.RemoveAll(name); err != nil { return fmt.Errorf("remove replacement %q: %w", name, err) }
            if err := syncRootDirectory(root, "."); err != nil { return err }
        }
    }
    for _, name := range j.OriginalNames {
        oldPath := filepath.Join(held, name)
        _, err := root.Lstat(oldPath)
        if errors.Is(err, os.ErrNotExist) {
            // Either evacuation had not reached this entry, or an earlier
            // recovery already moved it back. Keep it, but require it exists.
            if _, liveErr := root.Lstat(name); liveErr != nil {
                return fmt.Errorf("original %q missing from held and live trees: %w", name, liveErr)
            }
            continue
        }
        if err != nil { return fmt.Errorf("cannot inspect held original %q: %w", name, err) }
        if err := root.RemoveAll(name); err != nil { return fmt.Errorf("clear replacement %q: %w", name, err) }
        if err := syncRootDirectory(root, "."); err != nil { return err }
        if err := root.Rename(oldPath, name); err != nil { return fmt.Errorf("restore original %q: %w", name, err) }
        // Persist the restored destination entry before the source removal.
        if err := syncRootDirectory(root, "."); err != nil { return err }
        if err := syncRootDirectory(root, held); err != nil { return err }
    }
    return nil
}
```

### Mandatory commit/recovery ordering

Before PREPARED: finish bounded extraction, verify the archive and each admitted file, sync replacement file contents and directories, create/sync holding directories, record full inventories and model snapshots, then durably publish the private journal. During evacuation, move originals and sync the holding destination directory before the source directory. **Only after evacuation is durable** publish INSTALLING. Install replacements and sync the destination parent before the source parent; publish matching new metadata and fsync it; publish COMMITTED durably; only then is old-tree cleanup permitted.

For a noncommitted journal, restore the old tree with the helper, restore OldModel through publication-aware persistence, and durably record rollback completion before deleting staging. For a committed journal, verify identity and finish committed cleanup; never infer a rollback from an empty holding directory. A failed sync or ambiguous filesystem result keeps `RecoveryOperation` set. An unrecognized/legacy journal must be retained for migration/manual recovery, not guessed into one of these phases.

This design assumes the storage honors successful fsync/rename ordering and no uncooperative writer changes the tree. Network/unsupported filesystems need an explicit durability policy. Test the actual supported filesystems. A generation-directory/pointer architecture could simplify future atomic tree publication, but migrating live mount paths requires a separate design and data migration.

## H05. Bounded login attempt limiter

A configurable fixed-window baseline follows. It bounds both attempts and map cardinality. It deliberately fails closed for new keys when capacity is exhausted rather than evicting active buckets and allowing bypass by key churn. Account/IP/global policies must be balanced to avoid easy account-lockout or shared-NAT denial of service. A token-bucket/sliding-window policy can provide smoother boundary behavior; this is not a claim that a fixed window eliminates all brute-force risk.

```go
// Imports: fmt, net, sync, time.
type attemptBucket struct { Start time.Time; Count int }
type AttemptLimiter struct {
    mu sync.Mutex
    limit, maxKeys int
    window time.Duration
    buckets map[string]attemptBucket
}
func NewAttemptLimiter(limit, maxKeys int, window time.Duration) (*AttemptLimiter, error) {
    if limit <= 0 || maxKeys <= 0 || window <= 0 { return nil, fmt.Errorf("invalid attempt limits") }
    return &AttemptLimiter{limit: limit, maxKeys: maxKeys, window: window,
        buckets: make(map[string]attemptBucket)}, nil
}
func (l *AttemptLimiter) Allow(key string, now time.Time) bool {
    l.mu.Lock()
    defer l.mu.Unlock()
    bucket, exists := l.buckets[key]
    if !exists {
        if len(l.buckets) >= l.maxKeys {
            for k, value := range l.buckets {
                if now.Sub(value.Start) >= l.window { delete(l.buckets, k) }
            }
        }
        if len(l.buckets) >= l.maxKeys { return false }
        bucket.Start = now
    } else if now.Sub(bucket.Start) >= l.window {
        bucket = attemptBucket{Start: now}
    }
    if bucket.Count >= l.limit { return false }
    bucket.Count++
    l.buckets[key] = bucket
    return true
}
func peerIP(remote string) string {
    host, _, err := net.SplitHostPort(remote)
    if err != nil { return "invalid-peer" }
    ip := net.ParseIP(host)
    if ip == nil { return "invalid-peer" }
    return ip.String()
}
```

Do username/byte validation before constructing account keys; use an explicit trusted-proxy configuration if the real peer must be recovered from forwarding headers. Keep expensive derivation admission independent of attempt accounting so authenticated reset endpoints cannot saturate CPU. Test limiter behavior with a fake clock and concurrent callers before selecting production limits.

## H06. Typed MCP dispatcher with explicit scopes

This is a proposed replacement for the restricted raw-command capability, using the action constants from R08. `AgentBackend` is an integration interface: its implementations call the existing manager through the new operation/authorization layer. The dispatcher itself is complete and has no fallback to arbitrary console text.

```go
// Imports: context, fmt, time.
type AgentGrant struct {
    ID string
    ExpiresAt time.Time
    Servers map[string]bool
    Actions map[AgentAction]bool
}
type AgentBackend interface {
    ReadServer(context.Context, string) (any, error)
    StartServer(context.Context, string) (any, error)
    StopServer(context.Context, string) (any, error)
    RestartServer(context.Context, string) (any, error)
    BackupServer(context.Context, string) (any, error)
}
func dispatchAgent(ctx context.Context, grant AgentGrant, action AgentAction,
    serverID string, backend AgentBackend) (any, error) {
    if err := ctx.Err(); err != nil { return nil, err }
    if grant.ID == "" || (!grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt)) {
        return nil, fmt.Errorf("invalid or expired agent grant")
    }
    if !grant.Servers[serverID] || !grant.Actions[action] { return nil, fmt.Errorf("action is outside this token's scope") }
    switch action {
    case AgentRead: return backend.ReadServer(ctx, serverID)
    case AgentStart: return backend.StartServer(ctx, serverID)
    case AgentStop: return backend.StopServer(ctx, serverID)
    case AgentRestart: return backend.RestartServer(ctx, serverID)
    case AgentBackup: return backend.BackupServer(ctx, serverID)
    default: return nil, fmt.Errorf("unsupported agent action")
    }
}
```

Resolve the token and its current revocation/scopes at every request, audit the token ID rather than trusting a client actor string, and make backend data exposure obey the same server scope. A started operation records the grant identity; decide explicitly whether revocation cancels queued/in-progress operations. Existing tokens need an explicit migration policy.

## H07. Bounded, application-owned worker pool

This pool provides **in-memory execution admission**, not durable jobs or exactly-once effects. The durable operation journal must exist separately and keep pending/unknown outcomes across restart. Every submitted callback must respect context or the application cannot forcibly stop it. The owner must call Close on shutdown; queued jobs are finalized as canceled and running jobs receive cancellation.

```go
// Imports: context, fmt, sync.
type PoolJob struct {
    ID string
    Run func(context.Context) error
    Finish func(error) error // Persist outcome; called even after a Run panic.
}
type JobPool struct {
    mu sync.Mutex
    closed bool
    ctx context.Context
    cancel context.CancelFunc
    queue chan PoolJob
    wg sync.WaitGroup
    reportStoreError func(error)
}
func NewJobPool(parent context.Context, workers, queueSize int,
    reportStoreError func(error)) (*JobPool, error) {
    if workers <= 0 || queueSize <= 0 || reportStoreError == nil {
        return nil, fmt.Errorf("invalid worker pool configuration")
    }
    ctx, cancel := context.WithCancel(parent)
    p := &JobPool{ctx: ctx, cancel: cancel, queue: make(chan PoolJob, queueSize),
        reportStoreError: reportStoreError}
    for i := 0; i < workers; i++ {
        p.wg.Add(1)
        go func() {
            defer p.wg.Done()
            for job := range p.queue {
                result := p.ctx.Err()
                if result == nil {
                    result = safeJobCall(func() error { return job.Run(p.ctx) })
                }
                if err := safeJobCall(func() error { return job.Finish(result) }); err != nil {
                    p.reportStoreError(fmt.Errorf("job %s outcome persistence failed: %w", job.ID, err))
                }
            }
        }()
    }
    return p, nil
}
func safeJobCall(f func() error) (err error) {
    defer func() { if v := recover(); v != nil { err = fmt.Errorf("worker panic: %v", v) } }()
    return f()
}
func (p *JobPool) Submit(job PoolJob) error {
    if job.ID == "" || job.Run == nil || job.Finish == nil { return fmt.Errorf("invalid job") }
    p.mu.Lock()
    defer p.mu.Unlock()
    if p.closed || p.ctx.Err() != nil { return fmt.Errorf("worker pool is shutting down") }
    select {
    case p.queue <- job: return nil
    default: return fmt.Errorf("worker queue is full")
    }
}
func (p *JobPool) Close() {
    p.mu.Lock()
    if !p.closed {
        p.closed = true
        p.cancel()
        close(p.queue)
    }
    p.mu.Unlock()
    p.wg.Wait()
}
```

A durable pending occurrence stays pending when Submit reports full; it is retried by the dispatcher, not dropped. New HTTP work should be rejected before acceptance when its durable backlog budget is exhausted. Scope idempotency keys to authenticated actor, server, operation type and request digest. `Finish` must distinguish failed-before-publication from published-but-uncertain/recovery-required. Provide deadlines for state-store reporting too, and ensure the reporting callback cannot permanently block a worker.

## H08. Docker inspection that preserves unknown state

Use the ContainerState enum from R35. This adapter takes an already configured trusted Docker Engine HTTP client; for a local Unix socket, configure its transport explicitly, and for a remote daemon use authenticated TLS. The base URL is application configuration, never request input. Unversioned paths below should be replaced by the application's negotiated supported Engine API version where required.

```go
// Imports: context, encoding/json, fmt, io, net/http, net/url, strings, time.
type ContainerObservation struct {
    State ContainerState
    ID string
    Status string
    Labels map[string]string
}
func inspectContainer(ctx context.Context, client *http.Client, baseURL, id string) (ContainerObservation, error) {
    unknown := ContainerObservation{State: ContainerUnknown}
    if client == nil || id == "" { return unknown, fmt.Errorf("invalid Docker inspection configuration") }
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    endpoint := strings.TrimRight(baseURL, "/")+"/containers/"+url.PathEscape(id)+"/json"
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
    if err != nil { return unknown, err }
    res, err := client.Do(req)
    if err != nil { return unknown, err }
    defer res.Body.Close()
    if res.StatusCode == http.StatusNotFound {
        return ContainerObservation{State: ContainerMissing}, nil
    }
    if res.StatusCode != http.StatusOK { return unknown, fmt.Errorf("Docker inspect HTTP %d", res.StatusCode) }
    const maxBody = 2 << 20
    body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
    if err != nil { return unknown, err }
    if len(body) > maxBody { return unknown, fmt.Errorf("Docker inspect response too large") }
    var doc struct {
        ID string `json:"Id"`
        State struct {
            Running bool `json:"Running"`
            Restarting bool `json:"Restarting"`
            Status string `json:"Status"`
        } `json:"State"`
        Config struct { Labels map[string]string `json:"Labels"` } `json:"Config"`
    }
    if err := json.Unmarshal(body, &doc); err != nil { return unknown, err }
    if doc.ID == "" { return unknown, fmt.Errorf("Docker inspect omitted identity") }
    observation := ContainerObservation{State: ContainerUnknown, ID: doc.ID,
        Status: doc.State.Status, Labels: doc.Config.Labels}
    if doc.State.Restarting { return observation, nil }
    if doc.State.Running { observation.State = ContainerRunning; return observation, nil }
    if doc.State.Status == "exited" || doc.State.Status == "created" {
        observation.State = ContainerStopped
    }
    return observation, nil // Removing/dead/unrecognized states remain conservative.
}
```

Add ownership labels at creation (for example, a panel instance ID and server ID), verify them before destructive effects, and act on the immutable ID returned by inspection. Existing unlabeled containers need an explicit verified migration path. Extend the decoded observation with actual resolved host bindings, applied CPU/memory and mounts for R37/R38; do not derive them from desired settings. Do not log the entire inspect document because environment variables can contain secrets.

## H09. Reproduction evidence: what actually ran

The environment reported Go 1.23.2, Node 22.16.0 and OpenJDK 21.0.11. The tests below run on local temporary files/listeners and simulated control flow. They neither import Arcade's package nor execute its handlers. **Tests with names describing a defect intentionally pass when they observe the defective behavior.** A passing result is evidence for that primitive/interleaving, not evidence that the repository is fixed.

| Evidence | Executed cases | What it establishes | What it does not establish |
|---|---:|---|---|
| Go behavior proofs | 11 | Wildcard listener behavior, root-prefix error, checksum condition, tar overflow, FIFO blocking, split auth snapshot, credential boundary mismatch, restart-list logic, HTTP timeout/work separation, slice aliasing, CPU-unit arithmetic | Full Go 1.26 handler integration, production exploitability or all possible schedules |
| Node control-flow proofs | 6 | Stale mount ordering, Kick mode, disabled-task reactivation, invalid metadata enable predicate, ignored backup cancellation, stale task toggle overwrite | Actual DOM, browser, action runtime or game behavior |
| Reference helper tests | 6 | Success-path durable-write calls, digest/credential boundaries, independent restart merge, strict JSON cases, output cap, canceled wait | Power-loss durability, all filesystem failures or whole-repository compatibility |
| Java Properties reference cases | 3 | Colon separator, final duplicate value, continuation semantics | Every property syntax or each game's chosen loader |

### H09.1 Go behavior proof source

<!-- file: proofs_test.go -->
```go
package auditproofs

// Isolated language/runtime proofs transcribed from reviewed control flow.
// These are NOT the repository's tests and do not import its package.
import (
 "archive/tar"
 "bytes"
 "encoding/hex"
 "fmt"
 "io"
 "net"
 "net/http"
 "net/http/httptest"
 "net/netip"
 "os"
 "path/filepath"
 "strings"
 "syscall"
 "testing"
 "time"
)

func oldLoopback(host string) bool {
 switch host { case "", "localhost", "127.0.0.1", "::1": return true }
 ip := net.ParseIP(host)
 return ip != nil && ip.IsLoopback()
}
func safeNoAuthHost(host string) (string, error) {
 if host == "localhost" { return "127.0.0.1", nil }
 ip, err := netip.ParseAddr(host)
 if err != nil || !ip.Unmap().IsLoopback() { return "", fmt.Errorf("no-auth requires a literal loopback address") }
 return ip.String(), nil
}
func within(base, target string) bool {
 b, e1 := filepath.Abs(base); p, e2 := filepath.Abs(target)
 if e1 != nil || e2 != nil { return false }
 rel, err := filepath.Rel(b, p)
 return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func oldDigestChecks(value string) bool {
 b, err := hex.DecodeString(strings.TrimSpace(value))
 return err == nil && len(b) == 32
}
func parseDigest(value string) ([]byte, error) {
 value = strings.TrimSpace(value)
 if value == "" { return nil, nil }
 b, err := hex.DecodeString(value)
 if err != nil || len(b) != 32 { return nil, fmt.Errorf("sha256 must be exactly 64 hexadecimal characters") }
 return b, nil
}

func Test01EmptyHostBypassesLoopbackButBindsWildcard(t *testing.T) {
 if !oldLoopback("") { t.Fatal("expected reviewed predicate to allow blank") }
 ln, err := net.Listen("tcp", net.JoinHostPort("", "0")); if err != nil { t.Fatal(err) }; defer ln.Close()
 if !ln.Addr().(*net.TCPAddr).IP.IsUnspecified() { t.Fatalf("not wildcard: %s", ln.Addr()) }
 if _, err := safeNoAuthHost(""); err == nil { t.Fatal("fix allowed wildcard") }
 for _, h := range []string{"127.0.0.1", "127.9.8.7", "::1", "localhost"} {
  if _, err := safeNoAuthHost(h); err != nil { t.Fatalf("loopback %s: %v", h, err) }
 }
}
func Test02FilesystemRootEscapesStringAncestorCheck(t *testing.T) {
 source, data := "/", "/var/teploy-arcade"
 oldRejects := strings.HasPrefix(data, source+string(os.PathSeparator))
 if oldRejects { t.Fatal("expected // prefix mismatch") }
 if !within(source, data) { t.Fatal("relative path fix missed root ancestor") }
 if within("/data", "/data-other/world") { t.Fatal("sibling prefix accepted") }
}
func Test03MalformedDigestIsTreatedLikeOmittedDigest(t *testing.T) {
 for _, value := range []string{"not-a-sha", "aa", strings.Repeat("g",64)} {
  if oldDigestChecks(value) { t.Fatal("expected old guard to skip verification") }
  if _, err := parseDigest(value); err == nil { t.Fatalf("fix did not reject %q", value) }
 }
 if _, err := parseDigest(strings.Repeat("ab",32)); err != nil { t.Fatal(err) }
}
func Test04GrowingFileExceedsTarHeader(t *testing.T) {
 var dst bytes.Buffer
 tw := tar.NewWriter(&dst)
 if err := tw.WriteHeader(&tar.Header{Name:"growing.log", Mode:0600, Size:3}); err != nil { t.Fatal(err) }
 _, err := io.Copy(tw, strings.NewReader("oldnew"))
 if err != tar.ErrWriteTooLong { t.Fatalf("got %v", err) }
 var fixed bytes.Buffer; fw := tar.NewWriter(&fixed)
 if err := fw.WriteHeader(&tar.Header{Name:"growing.log",Mode:0600,Size:3}); err != nil { t.Fatal(err) }
 if _, err := io.CopyN(fw, strings.NewReader("oldnew"),3); err != nil { t.Fatal(err) }
 if err := fw.Close(); err != nil { t.Fatal(err) }
}
func Test05OrdinaryOpenBlocksOnFIFO(t *testing.T) {
 path := filepath.Join(t.TempDir(), "fifo")
 if err := syscall.Mkfifo(path,0600); err != nil { t.Fatal(err) }
 started := make(chan struct{}); opened := make(chan error,1)
 go func(){ close(started); f, err := os.Open(path); if f != nil { f.Close() }; opened<-err }()
 <-started
 select { case err := <-opened: t.Fatalf("ordinary open unexpectedly completed: %v",err); case <-time.After(40*time.Millisecond): }
 // Release the blocked reader within the isolated temporary directory.
 fd,err:=syscall.Open(path,syscall.O_WRONLY|syscall.O_NONBLOCK,0600); if err != nil { t.Fatal(err) }; syscall.Close(fd)
 select { case err:=<-opened: if err!=nil {t.Fatal(err)}; case <-time.After(time.Second): t.Fatal("cleanup timed out") }
 f,err:=os.OpenFile(path,os.O_RDONLY|syscall.O_NONBLOCK,0); if err != nil {t.Fatal(err)}; defer f.Close()
 fi,err:=f.Stat(); if err!=nil {t.Fatal(err)}; if fi.Mode().IsRegular() {t.Fatal("FIFO reported regular")}
}
func Test06SplitAuthenticationSnapshotAllowsTransition(t *testing.T) {
 users, setup := 0, true
 enabledBefore := users>0
 // First-account publication can occur between two individually locked reads.
 users, setup = 1, false
 oldAllows := !enabledBefore && !setup
 if !oldAllows || users!=1 { t.Fatal("failed to model the reviewed interleaving") }
 // An atomic snapshot made before the transition sees setup=true, and refuses.
 beforeUsers,beforeSetup:=0,true
 if beforeUsers==0 && !beforeSetup {t.Fatal("atomic before snapshot unexpectedly allows")}
}
func Test07CredentialCreationAndLoginLimitsDisagree(t *testing.T) {
 password:=strings.Repeat("x",1025)
 creationAccepts:=len(password)>=8
 loginAccepts:=len(password)>=1 && len(password)<=1024
 if !creationAccepts || loginAccepts {t.Fatal("expected accepted-at-create/rejected-at-login")}
}
func Test08PendingRestartPortComparisonAndOverwrite(t *testing.T) {
 props:=map[string]string{"server-port":"25565"}; changes:=map[string]string{"server-port":"25566"}
 props["server-port"]="25566" // changeServerPort happens before the settings comparison.
 need:=[]string{}
 for k,v:=range changes {if props[k]!=v {need=append(need,k)}}
 if len(need)!=0 {t.Fatal("expected lost port restart flag")}
 pending:=[]string{"memory_mb"}; pending=need
 if len(pending)!=0 {t.Fatal("expected prior pending changes to be erased")}
}
func Test09WriteTimeoutDoesNotCancelHandlerWorkAtDeadline(t *testing.T) {
 continued:=make(chan bool,1)
 srv:=httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  time.Sleep(90*time.Millisecond)
  continued <- r.Context().Err()==nil
  _,_=w.Write([]byte("changed after deadline"))
 }))
 srv.Config.WriteTimeout=15*time.Millisecond; srv.Start(); defer srv.Close()
 client:=srv.Client(); client.Timeout=time.Second
 resp,_:=client.Get(srv.URL); if resp!=nil {resp.Body.Close()}
 select {case ok:=<-continued: if !ok {t.Fatal("context was canceled before first late write")}; case <-time.After(time.Second):t.Fatal("handler did not finish")}
}
func Test10SliceSnapshotAliasesMutableBackingArray(t *testing.T) {
 pending:=[]string{"cpu","memory"}; snapshot:=pending
 pending[0]="server-port"
 if snapshot[0]!="server-port" {t.Fatal("expected alias")}
 copied:=append([]string(nil),pending...); pending[0]="other"
 if copied[0]=="other" {t.Fatal("copy still aliases")}
}
func Test11DockerCPUAndQuotaRelativeCPUAreDifferentUnits(t *testing.T) {
 dockerPercent, quota := 100.0,2.0 // one busy core in a two-core quota.
 oldHostCores:=dockerPercent/100*quota
 trueCores:=dockerPercent/100
 quotaPercent:=dockerPercent/quota
 if oldHostCores!=2 || trueCores!=1 || quotaPercent!=50 {t.Fatal("unit proof failed")}
}
```

### H09.2 JavaScript proof source

<!-- file: proofs.mjs -->
```javascript
// Isolated control-flow proofs; not a browser or the repository's JS suite.
import assert from 'node:assert/strict';
import test from 'node:test';
const deferred=()=>{let resolve; const promise=new Promise(r=>resolve=r); return {promise,resolve}};

test('12: late route A overwrites newer route B without a generation check',async()=>{
 let mounted='';const a=deferred(), b=deferred();
 const route=async(view)=>{mounted=await view.promise};
 const pa=route(a), pb=route(b);b.resolve('B');await pb;a.resolve('A');await pa;
 assert.equal(mounted,'A');
 let epoch=0;const fixed=async(view)=>{const mine=++epoch;const el=await view.promise;if(mine===epoch)mounted=el};
 const c=deferred(),d=deferred();const pc=fixed(c),pd=fixed(d);d.resolve('D');await pd;c.resolve('C');await pc;
 assert.equal(mounted,'D');
});
test('13: kick action inherits chat mode',()=>{
 const ctrl={chat:true,sendRaw(text){return {text,mode:this.chat?'say':'command'}}};
 assert.deepEqual(ctrl.sendRaw('kick AuditTestPlayer'),{text:'kick AuditTestPlayer',mode:'say'});
});
test('14: editing a disabled task enables it',()=>{
 const task={name:'nightly',enabled:false};
 const oldBody={name:'renamed',enabled:true};
 assert.equal({...task,...oldBody}.enabled,true);
 const fixedPatch={name:'renamed'};
 assert.equal({...task,...fixedPatch}.enabled,false);
});
test('15: metadata action rejects the literal enable=!is_prerelease',()=>{
 // This predicate is taken from docker/metadata-action v5 src/meta.ts.
 function validateEnabled(enabled){if(!['true','false'].includes(enabled))throw new Error(`Invalid value for enable attribute: ${enabled}`)}
 assert.throws(()=>validateEnabled('!is_prerelease'),/Invalid value/);
 validateEnabled('true');validateEnabled('false');
});
test('16: canceling the optional backup note still requests a backup',()=>{
 const promptResult=null;
 const note=promptResult||'';
 assert.equal(note,''); // current code proceeds unconditionally.
 const shouldCreate=promptResult!==null;
 assert.equal(shouldCreate,false);
});
test('17: a toggle PATCH containing a stale full task overwrites a concurrent edit',()=>{
 const stale={name:'nightly',commands:'say old',enabled:true};
 const current={...stale,commands:'say reviewed'};
 const oldPatch={...stale,enabled:false};
 assert.equal({...current,...oldPatch}.commands,'say old');
 assert.equal({...current,enabled:false}.commands,'say reviewed');
});
```

### H09.3 Java reference source

<!-- file: PropertyProof.java -->
```java
import java.io.StringReader;
import java.util.Properties;
public final class PropertyProof {
 public static void main(String[] args) throws Exception {
  String[] cases={"server-port:25566\n", "server-port=25565\nserver-port=bad\n", "server-port=255\\\n  66\n"};
  String[] expected={"25566","bad","25566"};
  for(int i=0;i<cases.length;i++){
   Properties p=new Properties();p.load(new StringReader(cases[i]));
   String actual=p.getProperty("server-port");
   if(!expected[i].equals(actual))throw new AssertionError(actual);
   System.out.println("Java properties case "+(i+1)+": "+actual);
  }
 }
}
```

### H09.4 Reference helper test source

<!-- file: reference_fixes_test.go -->
```go
package auditproofs

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReferenceAtomicStateWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.json")
	out, err := AtomicStateWrite(p, []byte("[]\n"), 0600)
	if err != nil || !out.Published || !out.Durable {
		t.Fatalf("%+v %v", out, err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "[]\n" {
		t.Fatal(string(b), err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
}
func TestReferenceDigestAndCredentials(t *testing.T) {
	for _, s := range []string{"x", strings.Repeat("x", 64), strings.Repeat("0", 62)} {
		if _, err := ExpectedSHA256(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
	if b, e := ExpectedSHA256(strings.Repeat("a", 64)); e != nil || len(b) != 32 {
		t.Fatal(b, e)
	}
	if e := CredentialBounds("admin", strings.Repeat("p", 1025)); e == nil {
		t.Fatal("accepted oversized password")
	}
	if e := CredentialBounds("admin", "correct-horse"); e != nil {
		t.Fatal(e)
	}
}
func TestReferencePendingIndependentCopy(t *testing.T) {
	a := []string{"memory"}
	got := MergePending(a, []string{"port", "memory"})
	if !reflect.DeepEqual(got, []string{"memory", "port"}) {
		t.Fatal(got)
	}
	got[0] = "changed"
	if a[0] != "memory" {
		t.Fatal("aliased")
	}
}
func TestReferenceStrictJSON(t *testing.T) {
	for _, body := range []string{`{"name":"ok"}{}`, `{"unknown":1}`, `{"name":"` + strings.Repeat("x", 128) + `"}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var dst struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst, 64); err == nil {
			t.Fatal("accepted", body)
		}
	}
}
func TestReferenceBoundedCommand(t *testing.T) {
	b, e := BoundedCommand(context.Background(), time.Second, 4, "printf", "abcdefgh")
	if e == nil || string(b) != "abcd" {
		t.Fatalf("%q %v", b, e)
	}
	b, e = BoundedCommand(context.Background(), time.Second, 8, "printf", "ok")
	if e != nil || string(b) != "ok" {
		t.Fatalf("%q %v", b, e)
	}
}
func TestReferenceCanceledWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitContext(ctx, time.Hour); err == nil {
		t.Fatal("wait did not cancel")
	}
}
```

### H09.5 Recorded commands and outputs

The behavior and helper Go selections were run separately; together they comprise 17 Go tests. The Node suite contains six tests. The Java program asserts all three expected values before printing them.

```sh
# From /mnt/data during the audit:
GO111MODULE=off go test -race -v ./audit_work
node --test /mnt/data/audit_work/proofs.mjs
javac /mnt/data/audit_work/PropertyProof.java
java -cp /mnt/data/audit_work PropertyProof
GO111MODULE=off go test -race -v ./audit_work -run '^TestReference'
```

Original eleven Go behavior-test results:

```text
=== RUN   Test01EmptyHostBypassesLoopbackButBindsWildcard
--- PASS: Test01EmptyHostBypassesLoopbackButBindsWildcard (0.00s)
=== RUN   Test02FilesystemRootEscapesStringAncestorCheck
--- PASS: Test02FilesystemRootEscapesStringAncestorCheck (0.00s)
=== RUN   Test03MalformedDigestIsTreatedLikeOmittedDigest
--- PASS: Test03MalformedDigestIsTreatedLikeOmittedDigest (0.00s)
=== RUN   Test04GrowingFileExceedsTarHeader
--- PASS: Test04GrowingFileExceedsTarHeader (0.00s)
=== RUN   Test05OrdinaryOpenBlocksOnFIFO
--- PASS: Test05OrdinaryOpenBlocksOnFIFO (0.04s)
=== RUN   Test06SplitAuthenticationSnapshotAllowsTransition
--- PASS: Test06SplitAuthenticationSnapshotAllowsTransition (0.00s)
=== RUN   Test07CredentialCreationAndLoginLimitsDisagree
--- PASS: Test07CredentialCreationAndLoginLimitsDisagree (0.00s)
=== RUN   Test08PendingRestartPortComparisonAndOverwrite
--- PASS: Test08PendingRestartPortComparisonAndOverwrite (0.00s)
=== RUN   Test09WriteTimeoutDoesNotCancelHandlerWorkAtDeadline
--- PASS: Test09WriteTimeoutDoesNotCancelHandlerWorkAtDeadline (0.09s)
=== RUN   Test10SliceSnapshotAliasesMutableBackingArray
--- PASS: Test10SliceSnapshotAliasesMutableBackingArray (0.00s)
=== RUN   Test11DockerCPUAndQuotaRelativeCPUAreDifferentUnits
--- PASS: Test11DockerCPUAndQuotaRelativeCPUAreDifferentUnits (0.00s)
PASS
ok  	_/mnt/data/audit_work	1.145s
```

JavaScript results:

```text
TAP version 13
# Subtest: 12: late route A overwrites newer route B without a generation check
ok 1 - 12: late route A overwrites newer route B without a generation check
  ---
  duration_ms: 0.828902
  type: 'test'
  ...
# Subtest: 13: kick action inherits chat mode
ok 2 - 13: kick action inherits chat mode
  ---
  duration_ms: 0.697423
  type: 'test'
  ...
# Subtest: 14: editing a disabled task enables it
ok 3 - 14: editing a disabled task enables it
  ---
  duration_ms: 0.111848
  type: 'test'
  ...
# Subtest: 15: metadata action rejects the literal enable=!is_prerelease
ok 4 - 15: metadata action rejects the literal enable=!is_prerelease
  ---
  duration_ms: 1.290579
  type: 'test'
  ...
# Subtest: 16: canceling the optional backup note still requests a backup
ok 5 - 16: canceling the optional backup note still requests a backup
  ---
  duration_ms: 0.161574
  type: 'test'
  ...
# Subtest: 17: a toggle PATCH containing a stale full task overwrites a concurrent edit
ok 6 - 17: a toggle PATCH containing a stale full task overwrites a concurrent edit
  ---
  duration_ms: 0.110537
  type: 'test'
  ...
1..6
# tests 6
# suites 0
# pass 6
# fail 0
# cancelled 0
# skipped 0
# todo 0
# duration_ms 57.768885
```

Java reference results:

```text
Java properties case 1: 25566
Java properties case 2: bad
Java properties case 3: 25566
```

Six reference-helper test results:

```text
=== RUN   TestReferenceAtomicStateWrite
--- PASS: TestReferenceAtomicStateWrite (0.00s)
=== RUN   TestReferenceDigestAndCredentials
--- PASS: TestReferenceDigestAndCredentials (0.00s)
=== RUN   TestReferencePendingIndependentCopy
--- PASS: TestReferencePendingIndependentCopy (0.00s)
=== RUN   TestReferenceStrictJSON
--- PASS: TestReferenceStrictJSON (0.00s)
=== RUN   TestReferenceBoundedCommand
--- PASS: TestReferenceBoundedCommand (0.00s)
=== RUN   TestReferenceCanceledWait
--- PASS: TestReferenceCanceledWait (0.00s)
PASS
ok  	_/mnt/data/audit_work	1.012s
```

### H09.6 Reproduce from this single Markdown file

Save this report as `teploy-arcade-audit.md`. The extractor writes only the five explicitly marked reproduction source files into a new local folder; it does not apply any proposed repository patches.

```sh
python3 - <<'PY_EXTRACT'
from pathlib import Path
import re
text = Path('teploy-arcade-audit.md').read_text()
out = Path('audit-proof-reproduction')
out.mkdir(exist_ok=True)
for name, language, body in re.findall(
    r'<!-- file: ([A-Za-z0-9_.-]+) -->\n```([A-Za-z]+)\n(.*?)\n```', text, re.S
):
    (out / name).write_text(body + '\n')
PY_EXTRACT
cd audit-proof-reproduction
GO111MODULE=off go test -race -count=1 -v
node --test proofs.mjs
javac PropertyProof.java
java PropertyProof
```

Expected totals: 17 Go tests, six Node tests and three Java reference cases. The five marked files were also extracted from the completed Markdown report and these commands were rerun successfully during final validation. Linux is required for the included FIFO test; the reference implementation also assumes the documented private state directory and normal local fsync behavior. None of these commands is a substitute for the Go 1.26 repository validation described above.

## Coverage, exclusions and prior-audit crosswalk

The source review followed the current production paths across the backend, the custom MCP implementation, browser controllers/views and build configuration. It was not an automated exhaustive line-by-line proof of every asset or test file.

| Area | Actual review coverage |
|---|---|
| Entrypoint/application | `cmd/teploy-arcade/main.go`, `internal/arcade/app.go` |
| Backend production paths | `auth.go`, `api.go`, `api_ext.go`, `files.go`, `backup.go`, `manager.go`, `model.go`, `runtime.go`, `runner.go`, `clone.go`, `import.go`, `plugins.go`, `mcp.go`, `scheduler.go`, `hub.go`, `players.go`, `java.go`, `metrics.go`, `hostcap.go`, `hostcap_linux.go`, `hostcap_darwin.go`, `templates.go` under `internal/arcade/` |
| MCP implementation | `internal/mcp/mcp.go`, `internal/mcp/tools.go` |
| Frontend | `app.js` main routing/console/settings/lifecycle code; `views.js` principal dashboard/files/backups/editor views; `views2.js`, `views-plugins.js`, `views-import.js`, `index.html` under `cmd/teploy-arcade/frontend/` |
| Build/deploy configuration | `go.mod`, `Dockerfile`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.goreleaser.yaml`, `teploy.yml` |
| Existing tests/audit history | Selected `internal/arcade/audit6_test.go` fixtures and the current `AUDIT_OPEN.md` register; other tests were inventoried but not comprehensively reviewed or run |
| Explicitly not claimed | Full CSS/visual/responsive/accessibility audit, every auxiliary frontend branch, every historical audit file, README/DEPLOY/Makefile verification, a dependency vulnerability inventory, browser execution, Docker runtime execution, deployed image inspection, a full Go 1.26 repository build/test, or real crash/power-loss reproduction |

Some large files were read in selected/ranged GitHub responses. Small auxiliary sections and complete frontend authentication dialogs were not exhaustively inspected. Source coverage is broad enough to identify the findings, but “file reviewed” must not be read as a formal proof over every line.

The current register already defers worker ownership, verified live snapshots, shared storage reservations, multi-resource transactions, rate/session controls, CSRF/proxy policy, pagination, deletion tombstones, Docker tri-state/executor/reconnect work, typed/scoped MCP, task cancellation, HTTPS plugin policy, durable async operations, and supply-chain/browser verification. This audit **corroborates** those themes and supplies implementation guidance; it does not pretend they were all unknown. Separate findings identify residual or alternate-path defects despite adjacent earlier fixes: empty-host validation, split setup-state observations, unreadable-held recovery cleanup, clone resume errors, properties grammar, restart-list handling, live-ledger reconstruction, malformed checksum input, native-image contracts and the literal release expression.

`teploy.yml` explicitly describes a proposed/not-yet-working deployment shape because of an upstream host-bind capability constraint. That documented limitation is **not** reported here as an undisclosed working-deployment bug. Likewise no guessed claim is made that Velocity categorically lacks the image's command bridge, and no vulnerability from a different JavaScript/WebSocket package is attributed to the Go dependency.

## Source reference directory

All Arcade links below resolve to the audited commit, not whichever `main` exists when this report is opened. External references were checked during this review and may move independently; pin image/action commits/digests during remediation.

- [`.github/workflows/ci.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.github/workflows/ci.yml)
- [`.github/workflows/release.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.github/workflows/release.yml)
- [`.goreleaser.yaml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/.goreleaser.yaml)
- [`AUDIT_OPEN.md`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/AUDIT_OPEN.md)
- [`Dockerfile`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/Dockerfile)
- [`cmd/teploy-arcade/frontend/app.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/app.js)
- [`cmd/teploy-arcade/frontend/index.html`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/index.html)
- [`cmd/teploy-arcade/frontend/views-import.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-import.js)
- [`cmd/teploy-arcade/frontend/views-plugins.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views-plugins.js)
- [`cmd/teploy-arcade/frontend/views.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views.js)
- [`cmd/teploy-arcade/frontend/views2.js`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/frontend/views2.js)
- [`cmd/teploy-arcade/main.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/cmd/teploy-arcade/main.go)
- [`go.mod`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/go.mod)
- [`internal/arcade/api.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api.go)
- [`internal/arcade/api_ext.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/api_ext.go)
- [`internal/arcade/app.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/app.go)
- [`internal/arcade/audit6_test.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/audit6_test.go)
- [`internal/arcade/auth.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/auth.go)
- [`internal/arcade/backup.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/backup.go)
- [`internal/arcade/clone.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/clone.go)
- [`internal/arcade/files.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/files.go)
- [`internal/arcade/hostcap.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap.go)
- [`internal/arcade/hostcap_darwin.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap_darwin.go)
- [`internal/arcade/hostcap_linux.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hostcap_linux.go)
- [`internal/arcade/hub.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/hub.go)
- [`internal/arcade/import.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/import.go)
- [`internal/arcade/java.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/java.go)
- [`internal/arcade/manager.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/manager.go)
- [`internal/arcade/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/mcp.go)
- [`internal/arcade/metrics.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/metrics.go)
- [`internal/arcade/model.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/model.go)
- [`internal/arcade/players.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/players.go)
- [`internal/arcade/plugins.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/plugins.go)
- [`internal/arcade/runner.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runner.go)
- [`internal/arcade/runtime.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/runtime.go)
- [`internal/arcade/scheduler.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/scheduler.go)
- [`internal/arcade/templates.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/arcade/templates.go)
- [`internal/mcp/mcp.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/mcp.go)
- [`internal/mcp/tools.go`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/internal/mcp/tools.go)
- [`teploy.yml`](https://github.com/useteploy/teploy-arcade/blob/329e67e73352aa7f972182ee02bd489d73234d8c/teploy.yml)

### Primary external references used

- [Go net listener address semantics](https://pkg.go.dev/net#Listen), [HTTP server/CrossOriginProtection](https://pkg.go.dev/net/http), [os.Root APIs](https://pkg.go.dev/os#Root), [archive/tar writer](https://pkg.go.dev/archive/tar#Writer.Write), and [Go vulnerability tooling](https://go.dev/doc/security/vuln/).
- [Java Properties API](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/Properties.html); the local reference program was executed on OpenJDK 21.
- [MCP base protocol](https://modelcontextprotocol.io/specification/2025-06-18/basic), [MCP HTTP transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports), and [JSON-RPC 2.0](https://www.jsonrpc.org/specification).
- [Docker Engine API](https://docs.docker.com/reference/api/engine/version/v1.46/) and [Docker stats CLI](https://docs.docker.com/reference/cli/docker/container/stats/).
- [docker/metadata-action v5 parser](https://github.com/docker/metadata-action/blob/v5/src/meta.ts), [tag parser](https://github.com/docker/metadata-action/blob/v5/src/tag.ts), and [context/input handling](https://github.com/docker/metadata-action/blob/v5/src/context.ts).
- [didstopia Rust Dockerfile](https://github.com/didstopia/rust-server/blob/master/Dockerfile), [Rust image documentation](https://hub.docker.com/r/didstopia/rust-server/), and [Valheim image documentation](https://hub.docker.com/r/lloesche/valheim-server).
- [Alpine release/support table](https://www.alpinelinux.org/releases/).

---

**Bottom line:** the repository contains meaningful prior defenses, but safety currently depends too heavily on the happy path and on assumptions that differ across handlers, restarts and game images. Close the concrete access/recovery/release defects first, then establish shared transactional, runtime-state, capability and operation models so subsequent fixes cannot leave another entry point behind. The report's code and tests are a remediation starting point, not a claim that an untested integrated patch has already been applied.
