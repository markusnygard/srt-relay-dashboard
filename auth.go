package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/websocket"
)

type User struct {
	User string `json:"user"`
	Pass string `json:"pass"`
	Role string `json:"role"`
}

// PublicUser is what the API returns: never the stored password.
type PublicUser struct {
	User string `json:"user"`
	Role string `json:"role"`
}

type authManager struct {
	mu       sync.Mutex
	file     string
	users    []User
	sessions map[string]string // token -> username
}

func newAuthManager(file string) (*authManager, error) {
	a := &authManager{
		file:     file,
		sessions: make(map[string]string),
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *authManager) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := os.ReadFile(a.file)
	if err != nil {
		if os.IsNotExist(err) {
			a.users = []User{{User: "admin", Role: "admin"}}
			if err := hashUserPass(&a.users[0], "admin"); err != nil {
				return err
			}
			return a.saveLocked()
		}
		return err
	}
	if err := json.Unmarshal(data, &a.users); err != nil {
		return err
	}
	if len(a.users) == 0 {
		a.users = []User{{User: "admin", Role: "admin"}}
		if err := hashUserPass(&a.users[0], "admin"); err != nil {
			return err
		}
	}
	// Upgrade any legacy plaintext entries to bcrypt.
	changed := false
	for i := range a.users {
		if !isBcrypt(a.users[i].Pass) && a.users[i].Pass != "" {
			if err := hashUserPass(&a.users[i], a.users[i].Pass); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		return a.saveLocked()
	}
	return nil
}

func (a *authManager) saveLocked() error {
	data, err := json.MarshalIndent(a.users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.file, data, 0o600)
}

func isBcrypt(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

func hashUserPass(u *User, plain string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Pass = string(h)
	return nil
}

func (a *authManager) listUsers() []PublicUser {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PublicUser, 0, len(a.users))
	for _, u := range a.users {
		out = append(out, PublicUser{User: u.User, Role: u.Role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

func (a *authManager) addUser(user, pass string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.User == user {
			return errUserExists
		}
	}
	nu := User{User: user, Role: "admin"}
	if err := hashUserPass(&nu, pass); err != nil {
		return err
	}
	a.users = append(a.users, nu)
	return a.saveLocked()
}

func (a *authManager) setPassword(user, pass string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.users {
		if a.users[i].User == user {
			if err := hashUserPass(&a.users[i], pass); err != nil {
				return err
			}
			return a.saveLocked()
		}
	}
	return errUserNotFound
}

func (a *authManager) removeUser(user string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.users) <= 1 {
		return errCannotRemoveLast
	}
	for i, u := range a.users {
		if u.User == user {
			a.users = append(a.users[:i], a.users[i+1:]...)
			for tok, un := range a.sessions {
				if un == user {
					delete(a.sessions, tok)
				}
			}
			return a.saveLocked()
		}
	}
	return errUserNotFound
}

func (a *authManager) authenticate(user, pass string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.users {
		u := &a.users[i]
		if u.User != user {
			continue
		}
		if isBcrypt(u.Pass) {
			if bcrypt.CompareHashAndPassword([]byte(u.Pass), []byte(pass)) == nil {
				return true
			}
		} else if u.Pass != "" && u.Pass == pass {
			// Legacy plaintext match: upgrade in place.
			if err := hashUserPass(u, pass); err == nil {
				a.saveLocked()
			}
			return true
		}
	}
	return false
}

func (a *authManager) createSession(username string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	a.sessions[token] = username
	return token
}

func (a *authManager) destroySession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *authManager) sessionUser(token string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[token]
}

var (
	errUserExists       = &apiError{code: http.StatusConflict, msg: "user already exists"}
	errUserNotFound     = &apiError{code: http.StatusNotFound, msg: "user not found"}
	errCannotRemoveLast = &apiError{code: http.StatusBadRequest, msg: "cannot remove the last user"}
)

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

const sessionCookieName = "srt_relay_session"

func (a *authManager) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   24 * 3600,
	})
}

func (a *authManager) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

func (a *authManager) sessionFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

type ctxKey int

const ctxUser ctxKey = 0

// requireAuth wraps an http.HandlerFunc, rejecting unauthenticated requests.
func (a *authManager) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := a.sessionFromRequest(r)
		username := a.sessionUser(token)
		if username == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, username)
		next(w, r.WithContext(ctx))
	}
}

// requireWS wraps a websocket handler, rejecting unauthenticated handshakes.
func (a *authManager) requireWS(next func(*websocket.Conn)) websocket.Handler {
	return websocket.Handler(func(ws *websocket.Conn) {
		r := ws.Request()
		token := a.sessionFromRequest(r)
		if a.sessionUser(token) == "" {
			ws.Write([]byte("unauthorized"))
			ws.Close()
			return
		}
		next(ws)
	})
}
