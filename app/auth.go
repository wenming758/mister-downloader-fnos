package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	roleAdmin    = "admin"
	roleOperator = "operator"
	sessionCookie = "mdm_fnos_session"
	pbkdf2Rounds  = 210000
	sessionTTL    = 7 * 24 * time.Hour
)

type User struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
	Created int64  `json:"created"`
}

type Config struct {
	Users       []User `json:"users"`
	DownloadDir string `json:"download_dir"`
}

type session struct {
	Name string
	Role string
	Exp  int64
}

type attempt struct {
	Count int
	Until int64
}

type Auth struct {
	mu       sync.Mutex
	path     string
	cfg      Config
	sessions map[string]session
	attempts map[string]*attempt
}

func loadAuth(path string) (*Auth, error) {
	a := &Auth{
		path:     path,
		sessions: map[string]session{},
		attempts: map[string]*attempt{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &a.cfg); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Auth) saveLocked() error {
	raw, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func (a *Auth) Initialized() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cfg.Users) > 0
}

func (a *Auth) DownloadDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DownloadDir
}

func (a *Auth) SetDownloadDir(dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.DownloadDir = dir
	return a.saveLocked()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var buf [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:])
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(password string) (salt, hash string) {
	s := randomHex(16)
	raw, _ := hex.DecodeString(s)
	sum := pbkdf2SHA256([]byte(password), raw, pbkdf2Rounds, 32)
	return s, hex.EncodeToString(sum)
}

func checkPassword(password, salt, hash string) bool {
	raw, err := hex.DecodeString(salt)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), raw, pbkdf2Rounds, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var errUserExists = errors.New("user exists")
var errLastAdmin = errors.New("last admin")
var errBadLogin = errors.New("bad login")

func (a *Auth) Authenticate(name, password string) (User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.cfg.Users {
		if u.Name == name && checkPassword(password, u.Salt, u.Hash) {
			return u, true
		}
	}
	return User{}, false
}

func (a *Auth) User(name string) (User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.cfg.Users {
		if u.Name == name {
			return u, true
		}
	}
	return User{}, false
}

func (a *Auth) Users() []User {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]User, 0, len(a.cfg.Users))
	for _, u := range a.cfg.Users {
		out = append(out, User{Name: u.Name, Role: u.Role, Created: u.Created})
	}
	return out
}

func (a *Auth) CreateInitial(name, password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cfg.Users) > 0 {
		return errUserExists
	}
	salt, hash := hashPassword(password)
	a.cfg.Users = append(a.cfg.Users, User{
		Name: name, Role: roleAdmin, Salt: salt, Hash: hash, Created: time.Now().Unix(),
	})
	return a.saveLocked()
}

func (a *Auth) AddUser(name, password, role string) error {
	if role != roleAdmin && role != roleOperator {
		role = roleOperator
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.cfg.Users {
		if a.cfg.Users[i].Name == name {
			if a.cfg.Users[i].Role == roleAdmin && role != roleAdmin && a.adminCountLocked() <= 1 {
				return errLastAdmin
			}
			if password != "" {
				salt, hash := hashPassword(password)
				a.cfg.Users[i].Salt = salt
				a.cfg.Users[i].Hash = hash
			}
			a.cfg.Users[i].Role = role
			return a.saveLocked()
		}
	}
	salt, hash := hashPassword(password)
	a.cfg.Users = append(a.cfg.Users, User{
		Name: name, Role: role, Salt: salt, Hash: hash, Created: time.Now().Unix(),
	})
	return a.saveLocked()
}

func (a *Auth) adminCountLocked() int {
	n := 0
	for _, u := range a.cfg.Users {
		if u.Role == roleAdmin {
			n++
		}
	}
	return n
}

func (a *Auth) DeleteUser(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, u := range a.cfg.Users {
		if u.Name != name {
			continue
		}
		if u.Role == roleAdmin && a.adminCountLocked() <= 1 {
			return errLastAdmin
		}
		a.cfg.Users = append(a.cfg.Users[:i], a.cfg.Users[i+1:]...)
		for tok, s := range a.sessions {
			if s.Name == name {
				delete(a.sessions, tok)
			}
		}
		return a.saveLocked()
	}
	return errBadLogin
}

func (a *Auth) NewSession(name, role string) string {
	tok := randomHex(32)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[tok] = session{Name: name, Role: role, Exp: time.Now().Add(sessionTTL).Unix()}
	return tok
}

func (a *Auth) DropSession(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, tok)
}

func (a *Auth) Session(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok {
		return session{}, false
	}
	if s.Exp < time.Now().Unix() {
		delete(a.sessions, c.Value)
		return session{}, false
	}
	s.Exp = time.Now().Add(sessionTTL).Unix()
	a.sessions[c.Value] = s
	return s, true
}

func (a *Auth) Allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().Unix()
	at, ok := a.attempts[ip]
	if !ok || at.Until < now {
		a.attempts[ip] = &attempt{Count: 1, Until: now + 60}
		return true
	}
	if at.Count >= 8 {
		return false
	}
	at.Count++
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func validName(name string) bool {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	return !strings.ContainsAny(name, "\r\n\t/\\")
}

func cleanDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}
