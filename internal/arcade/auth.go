package arcade

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Phase 8: authentication, roles and the audit log.
//
// Roles, narrowest first:
//
//	viewer  - read everything, change nothing
//	operator - start/stop/restart, run console commands, edit settings and files
//	admin   - the above plus create/delete servers, restore backups, manage users
//
// The console socket and every mutating route check the role. A viewer holding
// a session cannot start a server by POSTing directly.

const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

var roleRank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

type User struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Salt      string `json:"salt"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	// An admin who creates an account chooses its first password, and so knows
	// it. Until the owner replaces it, the account has a credential two people
	// hold. MustChange is what makes that temporary: every route except the
	// change itself is refused while it is set. Not set on the first admin,
	// who chose their own.
	MustChange bool `json:"must_change,omitempty"`
}

type Session struct {
	Token   string
	User    string
	Role    string
	Expires time.Time
}

type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

type Auth struct {
	mu       sync.RWMutex
	dataDir  string
	users    map[string]*User
	sessions map[string]*Session
	audit    []AuditEntry
	// Disabled when the panel binds to loopback and no password was ever set,
	// so a local run needs no ceremony. Any user created turns it on.
	enabled bool
	// forced records an explicit --no-auth, so creating a user does not
	// silently re-enable enforcement mid-process.
	forced bool

	// bootstrapToken gates the creation of the FIRST account while the panel
	// has none. Without it a fresh, reachable panel lets any visitor claim
	// admin - and on this panel admin means creating containers as root on the
	// host. Same design as teploy-dash: generated at startup, written to the
	// log and never to an HTTP response, and time-limited so an abandoned
	// setup does not leave the panel claimable indefinitely. It is implicitly
	// single-use: the first account turns enforcement on, after which /api/setup
	// refuses regardless of token.
	bootstrapToken  string
	bootstrapExpiry time.Time
	// setupGate is armed by BeginSetup on a panel with no accounts. Before it
	// existed, "no users" and "-no-auth" both meant !Enabled(), and the gate
	// below let every protected route through - so a freshly installed,
	// reachable panel exposed server creation, file access and MCP-token
	// issuance to anyone before the operator finished setup. While the gate is
	// armed and no account exists, protected routes answer 503; only health,
	// login-status and the token-gated setup route stay open. -no-auth (forced)
	// never arms it.
	setupGate bool
}

// bootstrapTokenTTL bounds how long a printed setup token stays valid. Long
// enough to copy it out of the log and finish setup in one sitting; short
// enough that a token in a log nobody reread is not a standing credential.
const bootstrapTokenTTL = 30 * time.Minute

const sessionTTL = 12 * time.Hour

func NewAuth(dataDir string) *Auth {
	return &Auth{
		dataDir:  dataDir,
		users:    map[string]*User{},
		sessions: map[string]*Session{},
	}
}

func (a *Auth) usersPath() string { return filepath.Join(a.dataDir, "users.json") }
func (a *Auth) auditPath() string { return filepath.Join(a.dataDir, "audit.json") }

// validateUserRecord rejects the records a syntactically valid JSON file can
// still hold: null elements, unknown roles, and credentials that cannot be the
// shape hashPassword writes. Loading used to dereference whatever decoded,
// so `[null]` in users.json panicked the panel at boot.
func validateUserRecord(u *User, i int) error {
	if u == nil {
		return fmt.Errorf("null user at index %d", i)
	}
	if strings.TrimSpace(u.Name) == "" {
		return fmt.Errorf("user at index %d has no name", i)
	}
	if _, ok := roleRank[u.Role]; !ok {
		return fmt.Errorf("user %q has unknown role %q", u.Name, u.Role)
	}
	salt, err1 := hex.DecodeString(u.Salt)
	hash, err2 := hex.DecodeString(u.Hash)
	if err1 != nil || err2 != nil || len(salt) != 16 || len(hash) != 32 {
		return fmt.Errorf("user %q has malformed stored credentials", u.Name)
	}
	return nil
}

func (a *Auth) Load() error {
	if b, err := os.ReadFile(a.usersPath()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Unreadable is not the same as absent. Absent means a fresh
			// install; unreadable means the file exists and the panel cannot
			// know who it describes, which is a repair-it-yourself state, not
			// a pretend-fresh one.
			quarantine(a.usersPath(), err)
			return fmt.Errorf("users file exists but cannot be read; repair %s: %w", a.usersPath(), err)
		}
	} else {
		var list []*User
		bad := json.Unmarshal(b, &list) != nil
		if !bad {
			seen := map[string]bool{}
			for i, u := range list {
				if err := validateUserRecord(u, i); err != nil {
					bad = true
					break
				}
				key := strings.ToLower(u.Name)
				if seen[key] {
					bad = true
					break
				}
				seen[key] = true
			}
		}
		if bad {
			// Quarantine rather than refuse to boot - but the empty user set
			// this leaves is NOT an open panel: BeginSetup arms the setup
			// gate, so every operational route stays closed until an admin is
			// claimed through the machine-local bootstrap token.
			quarantine(a.usersPath(), errors.New("invalid user records"))
		} else {
			for _, u := range list {
				a.users[strings.ToLower(u.Name)] = u
			}
		}
	}
	if b, err := os.ReadFile(a.auditPath()); err == nil {
		if err := json.Unmarshal(b, &a.audit); err != nil {
			quarantine(a.auditPath(), err)
		}
	}
	a.enabled = len(a.users) > 0
	return nil
}

// EnsureAdmin creates the first admin from credentials supplied at startup
// (-admin-user/-admin-password, or TEPLOY_ARCADE_ADMIN_USER/PASSWORD).
//
// This is the path teploy-dash takes with TEPLOY_DASH_PASSWORD, and it is the
// one most deployments should use: the operator provisions the account with the
// rest of the deploy and never meets the setup flow or its token at all. The
// token exists for the case where nobody provisioned anything.
//
// Reports whether it created an account. A panel that already has users is left
// alone - these credentials bootstrap a panel, they do not override one.
func (a *Auth) EnsureAdmin(name, password string) (bool, error) {
	if password == "" {
		return false, nil
	}
	if name == "" {
		name = "admin"
	}
	a.mu.RLock()
	existing := len(a.users)
	a.mu.RUnlock()
	if existing > 0 {
		return false, nil
	}
	if _, err := a.CreateFirstUser(name, password); err != nil {
		return false, err
	}
	return true, nil
}

// BeginSetup mints the token that gates first-run account creation, when the
// panel still has no accounts. A no-op once one exists. Arming the setup gate
// here is what turns "unclaimed" from an open panel into a closed one: Run
// always calls this (or provisions an admin, or was told --no-auth), so a
// production panel never serves operational routes before it is claimed.
func (a *Auth) BeginSetup() error {
	a.mu.RLock()
	n := len(a.users)
	a.mu.RUnlock()
	if n > 0 {
		return nil
	}
	tok, err := randomHex(24)
	if err != nil {
		// Fail closed. Without a token nobody can complete setup, which is far
		// better than falling back to an ungated one.
		return fmt.Errorf("could not generate a first-run setup token: %w", err)
	}
	a.mu.Lock()
	a.bootstrapToken = tok
	a.bootstrapExpiry = time.Now().Add(bootstrapTokenTTL)
	a.setupGate = true
	a.mu.Unlock()
	log.Printf("First-run setup required. Bootstrap token (valid %s): %s", bootstrapTokenTTL, tok)
	log.Printf("No accounts yet - this panel is unclaimed. Provision credentials with")
	log.Printf("-admin-user/-admin-password (or TEPLOY_ARCADE_ADMIN_USER/PASSWORD) to skip this.")
	log.Printf("Until an account is created, every other route answers 503.")
	return nil
}

// SetupRequired reports whether the panel is unclaimed with the setup gate
// armed: no accounts exist and setup has not been bypassed with --no-auth.
// Callers use it to refuse operational work (including MCP dispatch) rather
// than treating an unclaimed panel as a free-for-all.
func (a *Auth) SetupRequired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.setupGate && !a.forced && len(a.users) == 0
}

// CheckBootstrapToken reports whether supplied is the live, unexpired token.
//
// Constant-time against a real token. When there is no token or it has expired
// the comparison is skipped entirely, which discloses nothing: in that state
// there is no valid value to find.
func (a *Auth) CheckBootstrapToken(supplied string) bool {
	a.mu.RLock()
	tok, exp := a.bootstrapToken, a.bootstrapExpiry
	a.mu.RUnlock()
	if tok == "" || supplied == "" || time.Now().After(exp) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(tok)) == 1
}

// BootstrapExpired distinguishes "wrong token" from "the window closed", so the
// setup screen can tell an operator to restart the panel for a fresh one rather
// than leaving them retyping a token that can no longer work.
func (a *Auth) BootstrapExpired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.bootstrapToken != "" && time.Now().After(a.bootstrapExpiry)
}

// Disable turns auth off for a development run (--no-auth). It does not delete
// users; it only stops enforcement for this process.
func (a *Auth) Disable() {
	a.mu.Lock()
	a.enabled = false
	a.forced = true
	a.mu.Unlock()
}

// HasUsers reports whether any account exists, regardless of whether
// enforcement is switched on for this process. Enabled() answers "is auth being
// enforced"; this answers "is this panel claimed". -no-auth makes those differ.
func (a *Auth) HasUsers() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.users) > 0
}

func (a *Auth) Enabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.enabled
}

// pbkdf2Key is RFC 2898 PBKDF2-HMAC-SHA256. Implemented here rather than
// pulling golang.org/x/crypto in for twenty lines, so the agent stays on the
// standard library plus one websocket package.
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	var out []byte
	buf := make([]byte, 4)
	u := make([]byte, hashLen)

	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		t := prf.Sum(nil)
		copy(u, t)

		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw, salt string) string {
	return hex.EncodeToString(pbkdf2Key([]byte(pw), []byte(salt), 120_000, 32))
}

// randRead is a seam so the entropy-failure path can be exercised; every
// production caller reads crypto/rand.
var randRead = rand.Read

// randomHex reports a failed read instead of handing back the zero-filled
// buffer. Discarding that error is silent and total: every new user would get
// the same all-zeros salt and every new session the same all-zeros token, so one
// known password would unlock every account.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("system entropy is unavailable: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func checkNewUser(name, password, role string) error {
	if len(strings.TrimSpace(name)) < 2 {
		return fmt.Errorf("name must be at least 2 characters")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if _, ok := roleRank[role]; !ok {
		return fmt.Errorf("unknown role %q", role)
	}
	return nil
}

func (a *Auth) CreateUser(name, password, role string) (*User, error) {
	if err := checkNewUser(name, password, role); err != nil {
		return nil, err
	}
	// Hashing runs before the lock, like Login: createLocked used to run its
	// 120,000 PBKDF2 rounds with a.mu held for writing, stalling every
	// authenticated request for the duration.
	salt, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	hash := hashPassword(password, salt)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.createLocked(name, role, salt, hash, false)
}

// CreateFirstUser creates the first admin and refuses once anyone exists. The
// emptiness check has to happen under the same lock as the insert: /api/setup
// checked Enabled() and CreateUser took the lock separately, so two concurrent
// first-run requests both passed the gate and both got an admin - on an exposed
// panel, whoever races the operator gets an account of their own.
func (a *Auth) CreateFirstUser(name, password string) (*User, error) {
	if err := checkNewUser(name, password, RoleAdmin); err != nil {
		return nil, err
	}
	salt, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	hash := hashPassword(password, salt)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.users) > 0 {
		return nil, fmt.Errorf("this panel already has users; sign in instead")
	}
	return a.createLocked(name, RoleAdmin, salt, hash, true)
}

// createLocked must be called with a.mu held. Salt and hash arrive
// pre-computed so no KDF work happens inside the lock.
func (a *Auth) createLocked(name, role, salt, hash string, first bool) (*User, error) {
	key := strings.ToLower(name)
	if _, exists := a.users[key]; exists {
		return nil, fmt.Errorf("user %q already exists", name)
	}
	u := &User{Name: name, Role: role, Salt: salt,
		Hash: hash, CreatedAt: time.Now().Unix(),
		// The first admin picks their own password at setup and so is exempt;
		// createLocked's other caller is an admin creating someone else.
		MustChange: !first}
	next := make(map[string]*User, len(a.users)+1)
	for k, v := range a.users {
		cp := *v
		next[k] = &cp
	}
	next[key] = u
	if err := a.commitUsersLocked(next); err != nil {
		return nil, err
	}
	return u, nil
}

// commitUsersLocked persists a prospective user set and only then publishes it
// in memory. The old shape mutated a.users (or the pointed-to record) first
// and saved second, so a failed save left the panel acting on credentials the
// disk never got - a "failed" password change that actually changed it for
// this process, a "failed" user creation that exists until restart.
// Callers must hold a.mu.
func (a *Auth) commitUsersLocked(next map[string]*User) error {
	list := make([]*User, 0, len(next))
	for _, u := range next {
		list = append(list, u)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.usersPath(), b, 0o600); err != nil {
		return err
	}
	a.users = next
	a.enabled = !a.forced && len(next) > 0
	if len(next) > 0 {
		a.setupGate = false
	}
	return nil
}

// Login verifies a password and issues a session.
//
// The hashing happens with no lock held, which is the whole shape of this
// function. 120,000 PBKDF2 rounds take a deliberate ~100 ms, and this used to
// run them inside a.mu held for writing - the same mutex Session() takes on
// every authenticated request. One login stalled every request in flight; a
// dozen concurrent login attempts, which any bot that finds an exposed panel
// will produce, stalled it for over a second at a time. The cost is meant to
// slow down an attacker guessing passwords, not the operator watching a
// console.
//
// So: read the record under a read lock, release, hash, then take the write
// lock only to insert the session - three short critical sections instead of
// one long one.
func (a *Auth) Login(name, password string) (*Session, error) {
	// Copied, not borrowed. The map holds *User and SetPassword writes Salt and
	// Hash in place under the write lock, so carrying the pointer out of the
	// critical section and reading it while hashing is a real data race, not a
	// theoretical one. Four fields is a cheap copy.
	a.mu.RLock()
	u, ok := a.users[strings.ToLower(name)]
	var uname, role, salt, hash string
	if ok {
		uname, role, salt, hash = u.Name, u.Role, u.Salt, u.Hash
	}
	a.mu.RUnlock()

	if !ok {
		// Spend the same work on an unknown user so timing doesn't enumerate.
		hashPassword(password, "decoy")
		return nil, fmt.Errorf("invalid credentials")
	}

	want, got := []byte(hash), []byte(hashPassword(password, salt))
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return nil, fmt.Errorf("invalid credentials")
	}
	tok, err := randomHex(32)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	// Revalidate before minting: the PBKDF2 above ran unlocked for hundreds
	// of milliseconds, and a password change or account deletion landing in
	// that window would otherwise be rolled back by a fresh session minted
	// from the revoked credentials.
	u2, ok := a.users[strings.ToLower(name)]
	if !ok || u2.Salt != salt || u2.Hash != hash {
		a.mu.Unlock()
		return nil, fmt.Errorf("invalid credentials")
	}
	s := &Session{Token: tok, User: uname, Role: role,
		Expires: time.Now().Add(sessionTTL)}
	a.sessions[s.Token] = s
	a.mu.Unlock()
	return s, nil
}

func (a *Auth) Logout(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func (a *Auth) Session(token string) *Session {
	a.mu.RLock()
	s, ok := a.sessions[token]
	a.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(s.Expires) {
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
		return nil
	}
	return s
}

// reapSessions drops expired sessions. Session() only forgets one when it is
// looked up again, so without this the map grows for the life of the process.
func (a *Auth) reapSessions() {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for tok, s := range a.sessions {
		if now.After(s.Expires) {
			delete(a.sessions, tok)
		}
	}
}

func (a *Auth) Users() []map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]map[string]any, 0, len(a.users))
	for _, u := range a.users {
		out = append(out, map[string]any{
			"name": u.Name, "role": u.Role, "created_at": u.CreatedAt,
			"must_change": u.MustChange,
		})
	}
	return out
}

// SetPassword replaces a password.
//
// Two callers, one route. A user changing their own must present the current
// one - a stolen session should not be enough to lock the owner out of their
// own account - and clears MustChange by doing so. An admin resetting someone
// else's does not need it, and the reset sets MustChange, because the admin
// now knows a password they should not keep working with.
//
// keepToken is the caller's own session; every other session for that user is
// dropped. Changing a password is the move you make when you think someone
// else has it, and it means nothing if their session keeps working.
func (a *Auth) SetPassword(name, current, next, keepToken string, byAdmin bool) error {
	if len(next) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}

	// Both PBKDF2 derivations run OUTSIDE the mutex - the same shape Login
	// already uses. The old shape held the global write lock across up to
	// three 120,000-round derivations, so one viewer hammering self-service
	// password changes stalled every authenticated request in the panel.
	a.mu.RLock()
	key := strings.ToLower(name)
	u, ok := a.users[key]
	if !ok {
		a.mu.RUnlock()
		return fmt.Errorf("no such user")
	}
	type snapshot struct {
		name, salt, hash string
	}
	snap := snapshot{u.Name, u.Salt, u.Hash}
	a.mu.RUnlock()

	var currentOK bool
	if !byAdmin {
		got := hashPassword(current, snap.salt)
		currentOK = subtle.ConstantTimeCompare([]byte(snap.hash), []byte(got)) == 1
		if !currentOK {
			return fmt.Errorf("current password is incorrect")
		}
		if current == next {
			return fmt.Errorf("the new password is the same as the old one")
		}
	}

	salt, err := randomHex(16)
	if err != nil {
		return err
	}
	newHash := hashPassword(next, salt)

	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok = a.users[key]
	if !ok {
		return fmt.Errorf("no such user")
	}
	// Revalidate under the write lock: another password change may have
	// landed while this request hashed, and committing onto a stale
	// snapshot would silently revert it.
	if !byAdmin && (u.Salt != snap.salt || u.Hash != snap.hash) {
		return fmt.Errorf("the password changed underneath this request; try again")
	}

	nextUsers := make(map[string]*User, len(a.users))
	for k, v := range a.users {
		cp := *v
		nextUsers[k] = &cp
	}
	nextUsers[key].Salt = salt
	nextUsers[key].Hash = newHash
	nextUsers[key].MustChange = byAdmin
	if err := a.commitUsersLocked(nextUsers); err != nil {
		return err
	}

	for tok, s := range a.sessions {
		if strings.EqualFold(s.User, u.Name) && tok != keepToken {
			delete(a.sessions, tok)
		}
	}
	return nil
}

// MustChangePassword reports whether this account is holding a password
// somebody else chose for it.
func (a *Auth) MustChangePassword(name string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	u, ok := a.users[strings.ToLower(name)]
	return ok && u.MustChange
}

func (a *Auth) DeleteUser(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(name)
	u, ok := a.users[key]
	if !ok {
		return fmt.Errorf("no such user")
	}
	if u.Role == RoleAdmin {
		admins := 0
		for _, x := range a.users {
			if x.Role == RoleAdmin {
				admins++
			}
		}
		if admins <= 1 {
			return fmt.Errorf("refusing to delete the last admin")
		}
	}
	// Persist the prospective set before revoking anything: a failed save
	// used to delete the account in memory while the disk kept it, so the
	// "deleted" user simply returned after a restart - but their sessions
	// were already gone in this process, the worst of both outcomes.
	next := make(map[string]*User, len(a.users))
	for k, v := range a.users {
		if k == key {
			continue
		}
		cp := *v
		next[k] = &cp
	}
	if err := a.commitUsersLocked(next); err != nil {
		return err
	}
	for tok, s := range a.sessions {
		if strings.EqualFold(s.User, name) {
			delete(a.sessions, tok)
		}
	}
	return nil
}

// ---------------------------------------------------------------- audit

const auditKept = 2000

func (a *Auth) Append(e AuditEntry) {
	// Marshal and write inside the lock. Snapshotting under the lock and
	// writing outside it lets two appends interleave so the later write lands
	// an older snapshot, silently dropping an entry - and the audit log is the
	// only record of who did what.
	a.mu.Lock()
	defer a.mu.Unlock()

	a.audit = append(a.audit, e)
	if len(a.audit) > auditKept {
		a.audit = a.audit[len(a.audit)-auditKept:]
	}
	b, err := json.MarshalIndent(a.audit, "", "  ")
	if err != nil {
		log.Printf("audit: encode failed, entry not persisted: %v", err)
		return
	}
	if err := writeFileAtomic(a.auditPath(), b, 0o600); err != nil {
		log.Printf("audit: write failed, entry not persisted: %v", err)
	}
}

func (a *Auth) Audit(limit int) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if limit <= 0 || limit > len(a.audit) {
		limit = len(a.audit)
	}
	out := make([]AuditEntry, limit)
	copy(out, a.audit[len(a.audit)-limit:])
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---------------------------------------------------------- HTTP plumbing

type ctxKey string

const sessionKey ctxKey = "session"

func sessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(sessionKey).(*Session)
	return s
}

func actorOf(r *http.Request) string {
	if s := sessionFrom(r); s != nil {
		return s.User
	}
	return "local"
}

// require wraps a handler with a minimum role. When auth has never been set up
// the agent is single-user on loopback and everything is permitted; the moment
// a user exists, it is enforced.
func (a *Auth) require(role string, next http.HandlerFunc) http.HandlerFunc {
	return a.gate(role, true, next)
}

// requireEvenLockedOut is require() without the must-change lockout, for the
// one route a locked-out account has to be able to reach: the password change
// that clears the lock. Gating that too would leave the account unable to fix
// the condition that gates it.
func (a *Auth) requireEvenLockedOut(role string, next http.HandlerFunc) http.HandlerFunc {
	return a.gate(role, false, next)
}

func (a *Auth) gate(role string, lockout bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			if a.SetupRequired() {
				// Unclaimed, not development: the only routes that stay open
				// are the ones registered without this gate. Everything else
				// waits for the first admin, because "no users" must not mean
				// "administrator is whoever connected".
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error": "initial administrator setup is required before the panel can be used",
				})
				return
			}
			next(w, r)
			return
		}
		s := sessionFrom(r)
		if s == nil {
			writeJSON(w, 401, map[string]string{"error": "sign in required"})
			return
		}
		if roleRank[s.Role] < roleRank[role] {
			writeJSON(w, 403, map[string]string{
				"error": fmt.Sprintf("this needs the %s role; you are %s", role, s.Role)})
			return
		}
		// An account still holding the password an admin chose for it is
		// refused everything until it sets its own. Enforced here rather than
		// in the UI: the point is that the admin's copy of the password stops
		// being usable, and a screen the client draws does not achieve that.
		if lockout && a.MustChangePassword(s.User) {
			writeJSON(w, 403, map[string]any{
				"error":       "set your own password before using the panel",
				"must_change": true})
			return
		}
		next(w, r)
	}
}

func withSession(r *http.Request, s *Session) context.Context {
	return context.WithValue(r.Context(), sessionKey, s)
}

// attach resolves the session cookie for every request.
func (a *Auth) attach(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("gss_session"); err == nil {
			if s := a.Session(c.Value); s != nil {
				r = r.WithContext(withSession(r, s))
			}
		}
		next.ServeHTTP(w, r)
	})
}
