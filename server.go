package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
	"srt-relay-app/internal/relay"
)

//go:embed web/*
var webFS embed.FS

type server struct {
	relay        *relay.Relay
	auth         *authManager
	mu           sync.Mutex
	clients      map[*websocket.Conn]bool
	portLow      int
	portHigh     int
	egressOffset int
	publicHost   string
}

func newServer(r *relay.Relay, auth *authManager, portLow, portHigh, egressOffset int, publicHost string) *server {
	s := &server{
		relay:        r,
		auth:         auth,
		clients:      make(map[*websocket.Conn]bool),
		portLow:      portLow,
		portHigh:     portHigh,
		egressOffset: egressOffset,
		publicHost:   publicHost,
	}
	r.SetOnChange(s.broadcast)
	return s
}

func (s *server) broadcast(st *relay.Stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"type": "stream", "stream": st})
	for c := range s.clients {
		if _, err := c.Write(data); err != nil {
			delete(s.clients, c)
		}
	}
}

func (s *server) handleWS(ws *websocket.Conn) {
	s.mu.Lock()
	s.clients[ws] = true
	s.mu.Unlock()

	for _, st := range s.relay.ListStreams() {
		data, _ := json.Marshal(map[string]any{"type": "stream", "stream": st})
		ws.Write(data)
	}

	var msg string
	for {
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
	}

	s.mu.Lock()
	delete(s.clients, ws)
	s.mu.Unlock()
	ws.Close()
}

type addStreamRequest struct {
	Name   string `json:"name"`
	InPort int    `json:"inPort"`
}

func (s *server) handleAddStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req addStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if s.relay.PortInUse(req.InPort) || s.relay.PortInUse(req.InPort+s.egressOffset) {
		http.Error(w, "port already in use", http.StatusConflict)
		return
	}
	if req.InPort < s.portLow || req.InPort > s.portHigh {
		http.Error(w, fmt.Sprintf("ingress port must be %d-%d", s.portLow, s.portHigh), http.StatusBadRequest)
		return
	}

	streamID := generateStreamID(req.Name)
	st := s.relay.AddStream(req.Name, streamID, req.InPort, req.InPort+s.egressOffset)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *server) handleRemoveStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/streams/")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	s.relay.RemoveStream(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	streams := s.relay.ListStreams()
	sort.Slice(streams, func(i, j int) bool { return streams[i].InPort < streams[j].InPort })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(streams)
}

func (s *server) handleFreePorts(w http.ResponseWriter, r *http.Request) {
	var free []int
	for p := s.portLow; p <= s.portHigh; p++ {
		if !s.relay.PortInUse(p) && !s.relay.PortInUse(p+s.egressOffset) {
			free = append(free, p)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(free)
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"host":         s.publicHost,
		"portLow":      s.portLow,
		"portHigh":     s.portHigh,
		"egressOffset": s.egressOffset,
	})
}

// --- authentication handlers ---

type loginRequest struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.auth.authenticate(req.User, req.Pass) {
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	token := s.auth.createSession(req.User)
	s.auth.setSessionCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"user": req.User, "role": "admin"})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := s.auth.sessionFromRequest(r); tok != "" {
		s.auth.destroySession(tok)
	}
	s.auth.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	username := r.Context().Value(ctxUser).(string)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"user": username, "role": "admin"})
}

// --- user management ---

type addUserRequest struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.auth.listUsers())
}

func (s *server) handleAddUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req addUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.User == "" || req.Pass == "" {
		http.Error(w, "user and pass required", http.StatusBadRequest)
		return
	}
	err := s.auth.addUser(req.User, req.Pass)
	if ae, ok := err.(*apiError); ok && ae.code == http.StatusConflict {
		// User exists: treat as password change.
		if err := s.auth.setPassword(req.User, req.Pass); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err != nil {
		http.Error(w, "failed to add user", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.auth.listUsers())
}

func (s *server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := strings.TrimPrefix(r.URL.Path, "/api/users/")
	user = strings.TrimSuffix(user, "/password")
	if user == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	var req addUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Pass == "" {
		http.Error(w, "pass required", http.StatusBadRequest)
		return
	}
	if err := s.auth.setPassword(user, req.Pass); err != nil {
		if ae, ok := err.(*apiError); ok {
			http.Error(w, ae.msg, ae.code)
			return
		}
		http.Error(w, "failed to set password", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.auth.listUsers())
}

func (s *server) handleRemoveUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := strings.TrimPrefix(r.URL.Path, "/api/users/")
	if user == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	if err := s.auth.removeUser(user); err != nil {
		ae, ok := err.(*apiError)
		if ok {
			http.Error(w, ae.msg, ae.code)
			return
		}
		http.Error(w, "failed to remove user", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.auth.listUsers())
}

func (s *server) handleUserPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/users/")
	switch {
	case strings.HasSuffix(path, "/password"):
		s.handleSetPassword(w, r)
	default:
		s.handleRemoveUser(w, r)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.auth.requireAuth(s.handleLogout))
	mux.HandleFunc("/api/me", s.auth.requireAuth(s.handleMe))
	mux.HandleFunc("/api/users", s.auth.requireAuth(s.handleListUsers))
	mux.HandleFunc("/api/users/add", s.auth.requireAuth(s.handleAddUser))
	mux.HandleFunc("/api/users/", s.auth.requireAuth(s.handleUserPath))
	mux.HandleFunc("/api/config", s.auth.requireAuth(s.handleConfig))
	mux.HandleFunc("/api/streams", s.auth.requireAuth(s.handleListStreams))
	mux.HandleFunc("/api/streams/add", s.auth.requireAuth(s.handleAddStream))
	mux.HandleFunc("/api/streams/", s.auth.requireAuth(s.handleRemoveStream))
	mux.HandleFunc("/api/ports/free", s.auth.requireAuth(s.handleFreePorts))
	mux.Handle("/ws", s.auth.requireWS(s.handleWS))

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func generateStreamID(name string) string {
	clean := strings.ToLower(strings.TrimSpace(name))
	clean = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, clean)
	return clean
}

func startServer(r *relay.Relay, auth *authManager, addr string, portLow, portHigh, egressOffset int, publicHost string) {
	s := newServer(r, auth, portLow, portHigh, egressOffset, publicHost)
	log.Printf("web ui on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, s.routes()))
}
