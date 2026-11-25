package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/argon2"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/paths"
)

// Password lengths in bytes (design 0005 D6).
const (
	MinPasswordLen = 12
	MaxPasswordLen = 128
)

// Argon2id parameters: OWASP's minimum (design 0005 D6).
const (
	argonTime    = 2
	argonMemory  = 19456 // KiB
	argonThreads = 1
	saltLen      = 16
	keyLen       = 32
	// phcParams is the only parameter set this version writes and accepts.
	phcParams = "m=19456,t=2,p=1"
	// maxPasswordFile bounds what the service reads from the password file.
	maxPasswordFile = 4 << 10
)

// phcB64 is the PHC string format's base64: standard alphabet, no padding.
var phcB64 = base64.RawStdEncoding.Strict()

// CheckPasswordLength refuses a password outside 12–128 bytes.
func CheckPasswordLength(n int) error {
	switch {
	case n < MinPasswordLen:
		return fmt.Errorf("the password must be at least %d bytes; it has %d", MinPasswordLen, n)
	case n > MaxPasswordLen:
		return fmt.Errorf("the password must be at most %d bytes; it has %d", MaxPasswordLen, n)
	}
	return nil
}

// HashPassword returns the PHC string of password with a new random salt:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>.
func HashPassword(password []byte) (string, error) {
	if err := CheckPasswordLength(len(password)); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(password, salt, argonTime, argonMemory, argonThreads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, phcParams,
		phcB64.EncodeToString(salt), phcB64.EncodeToString(key)), nil
}

// passwordHash is a parsed PHC string.
type passwordHash struct {
	salt, key []byte
}

// parsePHC accepts exactly the form HashPassword writes, so a file with
// other parameters cannot raise the memory one verification takes.
func parsePHC(s string) (passwordHash, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return passwordHash{}, errors.New("not an Argon2id PHC string")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) || parts[3] != phcParams {
		return passwordHash{}, fmt.Errorf("unsupported parameters %s$%s; want v=%d$%s", parts[2], parts[3], argon2.Version, phcParams)
	}
	salt, err := phcB64.DecodeString(parts[4])
	if err != nil || len(salt) != saltLen {
		return passwordHash{}, errors.New("invalid salt")
	}
	key, err := phcB64.DecodeString(parts[5])
	if err != nil || len(key) != keyLen {
		return passwordHash{}, errors.New("invalid key")
	}
	return passwordHash{salt: salt, key: key}, nil
}

// verify derives the key for password and compares it in constant time.
func (h passwordHash) verify(password []byte) bool {
	got := argon2.IDKey(password, h.salt, argonTime, argonMemory, argonThreads, keyLen)
	return subtle.ConstantTimeCompare(got, h.key) == 1
}

// WritePasswordFile writes the PHC string to path with the atomic write of
// design 0004 D7 and mode 0600, creating missing directories with mode
// 0700 (design 0005 D6, 0006 D1).
func WritePasswordFile(path, phc string) error {
	if err := paths.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	if err := config.WriteAtomic(path, []byte(phc+"\n"), config.WriteOptions{}); err != nil {
		return err
	}
	// The temporary file is created with mode 0600 less the umask; make the
	// mode exact whatever the umask was.
	return os.Chmod(path, 0o600)
}

// passwordState is what one read of the password file found.
type passwordState struct {
	hash passwordHash
	// digest is the SHA-256 of the file content; sessions store it at login
	// (design 0005 D7).
	digest [sha256.Size]byte
	// problem says why login is disabled; "" when the file is usable.
	problem string
}

func (p passwordState) usable() bool { return p.problem == "" }

// fileKey identifies one version of the password file; the digest is
// cached by it (design 0005 D7).
type fileKey struct {
	dev, ino uint64
	size     int64
	mtime    int64
}

// passwordFile reads the password file at each use and caches its parsed
// content by inode, size, and modification time.
type passwordFile struct {
	path string

	mu     sync.Mutex
	cached bool
	key    fileKey
	state  passwordState
}

// load returns the file's current state. A file that is missing,
// unreadable, not a regular file, not mode 0600, owned by another user, or
// not a valid PHC string disables login (design 0005 D6).
func (p *passwordFile) load() passwordState {
	fi, err := os.Stat(p.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return passwordState{problem: "no password is set"}
	case err != nil:
		return passwordState{problem: "the password file cannot be read: " + err.Error()}
	case !fi.Mode().IsRegular():
		return passwordState{problem: "the password file is not a regular file"}
	}
	f, err := os.Open(p.path)
	if err != nil {
		return passwordState{problem: "the password file cannot be read: " + err.Error()}
	}
	defer f.Close()
	// Check the opened file itself, so the checks and the content belong to
	// the same version even if the file is replaced meanwhile.
	fi, err = f.Stat()
	if err != nil {
		return passwordState{problem: "the password file cannot be read: " + err.Error()}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return passwordState{problem: "the password file is not a regular file"}
	case fi.Mode().Perm() != 0o600:
		return passwordState{problem: fmt.Sprintf("the password file has mode %04o, not 0600", fi.Mode().Perm())}
	case !ok:
		return passwordState{problem: "the password file's owner is unknown"}
	case int(st.Uid) != os.Getuid():
		return passwordState{problem: "the password file is owned by another user"}
	}
	key := fileKey{dev: uint64(st.Dev), ino: st.Ino, size: fi.Size(), mtime: fi.ModTime().UnixNano()}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached && p.key == key {
		return p.state
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPasswordFile+1))
	var state passwordState
	switch {
	case err != nil:
		state.problem = "the password file cannot be read: " + err.Error()
	case len(data) > maxPasswordFile:
		state.problem = "the password file is too large"
	default:
		state.digest = sha256.Sum256(data)
		state.hash, err = parsePHC(strings.TrimSpace(string(data)))
		if err != nil {
			state.problem = "the password file is invalid: " + err.Error()
		}
	}
	p.cached, p.key, p.state = true, key, state
	return state
}

// PasswordProblem reports why the password file at path disables login, or
// "" when login is possible. cuttle serve logs it at start (design 0006 D5).
func PasswordProblem(path string) string {
	return (&passwordFile{path: path}).load().problem
}
