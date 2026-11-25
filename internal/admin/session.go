package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"sync"
	"time"
)

// Session and login limits (design 0005 D7, D8).
const (
	IdleTimeout     = 30 * time.Minute
	AbsoluteTimeout = 12 * time.Hour
	// LoginTokenTTL is the lifetime of the cuttle_login cookie and the
	// form token it pairs with.
	LoginTokenTTL = 10 * time.Minute
	// backoffFree is the number of consecutive failures before attempts
	// must wait; backoffCap bounds the wait.
	backoffFree = 3
	backoffCap  = 300 * time.Second
)

var tokenB64 = base64.RawURLEncoding

// randomToken returns a 256-bit random value in URL-safe base64.
func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("admin: crypto/rand failed: " + err.Error())
	}
	return tokenB64.EncodeToString(b[:])
}

// tokenEqual compares two tokens in constant time; empty tokens never
// match.
func tokenEqual(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// session is one signed-in browser (design 0005 D7).
type session struct {
	// csrf is the token every form of this session carries (design 0005
	// D8).
	csrf string
	// digest is the SHA-256 of the password file at login; a different
	// file ends the session.
	digest   [sha256.Size]byte
	created  time.Time
	lastSeen time.Time
}

// sessions is the in-memory session store. A restart ends every session.
type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

// create starts a session with a new ID and CSRF token.
func (s *sessions) create(now time.Time, digest [sha256.Size]byte) (string, session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*session{}
	}
	for id, v := range s.m {
		if v.expired(now) {
			delete(s.m, id)
		}
	}
	id := randomToken()
	v := &session{csrf: randomToken(), digest: digest, created: now, lastSeen: now}
	s.m[id] = v
	return id, *v
}

func (v *session) expired(now time.Time) bool {
	return now.Sub(v.lastSeen) >= IdleTimeout || now.Sub(v.created) >= AbsoluteTimeout
}

// get returns the live session with this ID and marks it used. A session
// past its idle or absolute timeout, or whose password file digest differs
// from the current one, or any session while login is disabled, ends here.
func (s *sessions) get(id string, now time.Time, pw passwordState) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return session{}, false
	}
	if v.expired(now) || !pw.usable() || v.digest != pw.digest {
		delete(s.m, id)
		return session{}, false
	}
	v.lastSeen = now
	return *v, true
}

// remove ends a session.
func (s *sessions) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// loginTokens mint and check the value that GET /login puts both in the
// cuttle_login cookie and in the form (design 0005 D8). A token carries
// its expiry and an HMAC under a key that lives only in this process, so
// the server enforces the 10-minute lifetime and accepts only tokens it
// issued, without storing them.
type loginTokens struct {
	key [32]byte
}

func newLoginTokens() *loginTokens {
	t := &loginTokens{}
	if _, err := rand.Read(t.key[:]); err != nil {
		panic("admin: crypto/rand failed: " + err.Error())
	}
	return t
}

const (
	loginNonceLen = 16
	loginTokenLen = loginNonceLen + 8 + sha256.Size
)

func (t *loginTokens) mint(now time.Time) string {
	b := make([]byte, loginNonceLen+8, loginTokenLen)
	if _, err := rand.Read(b[:loginNonceLen]); err != nil {
		panic("admin: crypto/rand failed: " + err.Error())
	}
	binary.BigEndian.PutUint64(b[loginNonceLen:], uint64(now.Add(LoginTokenTTL).Unix()))
	return tokenB64.EncodeToString(t.sign(b))
}

func (t *loginTokens) sign(b []byte) []byte {
	m := hmac.New(sha256.New, t.key[:])
	m.Write(b[:loginNonceLen+8])
	return m.Sum(b[:loginNonceLen+8])
}

// valid reports whether tok is a token this process minted that has not
// expired.
func (t *loginTokens) valid(tok string, now time.Time) bool {
	b, err := tokenB64.DecodeString(tok)
	if err != nil || len(b) != loginTokenLen {
		return false
	}
	want := t.sign(append([]byte(nil), b[:loginNonceLen+8]...))
	if !hmac.Equal(b, want) {
		return false
	}
	return now.Unix() < int64(binary.BigEndian.Uint64(b[loginNonceLen:]))
}

// backoff is the login failure state (design 0005 D8): after n
// consecutive failures, n >= 3, an attempt sooner than 2^(n-3) seconds
// after the last failure is refused, with the wait capped at 300 s.
type backoff struct {
	failures int
	last     time.Time
}

// backoffDelay is the wait after n consecutive failures. The exponent is
// bounded first so the shift cannot overflow; 2^16 s is past the cap.
func backoffDelay(n int) time.Duration {
	if n < backoffFree {
		return 0
	}
	e := min(n-backoffFree, 16)
	return min(time.Duration(1<<e)*time.Second, backoffCap)
}

// wait returns how long an attempt at now must still wait, or 0.
func (b *backoff) wait(now time.Time) time.Duration {
	if b.failures < backoffFree {
		return 0
	}
	return max(b.last.Add(backoffDelay(b.failures)).Sub(now), 0)
}

func (b *backoff) fail(now time.Time) int {
	b.failures++
	b.last = now
	return b.failures
}

func (b *backoff) reset() { b.failures = 0 }
