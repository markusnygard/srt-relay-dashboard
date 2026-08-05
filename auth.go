package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"sync"

	"golang.org/x/net/websocket"
)

type User struct {
	User string `json:"user"`
	Pass string `json:"pass"`
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
			a.users = []User{{User: "admin", Pass: "admin", Role: "admin"}}
			return a.saveLocked()
		}
		return err
	}
	if err := json.Unmarshal(data, &a.users); err != nil {
		return err
	}
	if len(a.users) == 0 {
		a.users = []User{{User: "admin", Pass: "admin", Role: "admin"}}
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

func (a *authManager) listUsers() []User {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]User, len(a.users))
	copy(out, a.users)
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
	a.users = append(a.users, User{User: user, Pass: pass, Role: "admin"})
	return a.saveLocked()
}

func (a *authManager) removeUser(user string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Never allow removing the last user.
	if len(a.users) <= 1 {
		return errCannotRemoveLast
	}
	for i, u := range a.users {
		if u.User == user {
			a.users = append(a.users[:i], a.users[i+1:]...)
			// drop their sessions
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
	for _, u := range a.users {
		if u.User == user && u.Pass == pass {
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
