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
	"time"

	"golang.org/x/net/websocket"
	"srt-relay-app/internal/relay"
)

//go:embed web/*
var webFS embed.FS

// serverTimeZone is the server's local timezone, exposed to the UI so it can
// show server-local time alongside browser-local time.
var serverTimeZone string

type server struct {
	relay        *relay.Relay
	auth         *authManager
	mu           sync.Mutex
	clients      map[*websocket.Conn]bool
	viewClients  map[*websocket.Conn]bool
	portLow      int
	portHigh     int
	egressOffset int
	publicHost   string
	viewEnabled  bool
}

func newServer(r *relay.Relay, auth *authManager, portLow, portHigh, egressOffset int, publicHost string, viewEnabled bool) *server {
	s := &server{
		relay:        r,
		auth:         auth,
		clients:      make(map[*websocket.Conn]bool),
		viewClients:  make(map[*websocket.Conn]bool),
		portLow:      portLow,
		portHigh:     portHigh,
		egressOffset: egressOffset,
		publicHost:   publicHost,
		viewEnabled:  viewEnabled,
	}
	r.SetOnChange(s.broadcast)
	r.SetOnRemove(s.broadcastRemove)
	return s
}

func (s *server) broadcastRemove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"type": "removed", "id": id})
	for c := range s.clients {
		if _, err := c.Write(data); err != nil {
			delete(s.clients, c)
		}
	}
	for c := range s.viewClients {
		if _, err := c.Write(data); err != nil {
			delete(s.viewClients, c)
		}
	}
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
	vdata, _ := json.Marshal(st)
	for c := range s.viewClients {
		if _, err := c.Write(vdata); err != nil {
			delete(s.viewClients, c)
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

// handleViewWS is the public read-only WebSocket used by the infoscreen page.
func (s *server) handleViewWS(ws *websocket.Conn) {
	s.mu.Lock()
	s.viewClients[ws] = true
	s.mu.Unlock()

	for _, st := range s.relay.ListStreams() {
		data, _ := json.Marshal(st)
		ws.Write(data)
	}

	var msg string
	for {
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
	}

	s.mu.Lock()
	delete(s.viewClients, ws)
	s.mu.Unlock()
	ws.Close()
}

type addStreamRequest struct {
	Name       string     `json:"name"`
	InPort     int        `json:"inPort"`
	StartAt    *time.Time `json:"startAt"`
	StopAt     *time.Time `json:"stopAt"`
	Recurrence string     `json:"recurrence"`
	AutoRemove bool       `json:"autoRemove"`
	Contact    string     `json:"contact"`
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
	if s.relay.PortClaimed(req.InPort) || s.relay.PortClaimed(req.InPort+s.egressOffset) {
		http.Error(w, "port already in use", http.StatusConflict)
		return
	}
	if req.InPort < s.portLow || req.InPort > s.portHigh {
		http.Error(w, fmt.Sprintf("ingress port must be %d-%d", s.portLow, s.portHigh), http.StatusBadRequest)
		return
	}

	streamID := generateStreamID(req.Name)
	st := s.relay.AddStream(req.Name, streamID, req.InPort, req.InPort+s.egressOffset,
		req.StartAt, req.StopAt, req.Recurrence, req.AutoRemove, req.Contact)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *server) handlePatchStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/streams/")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	st := s.relay.GetStream(id)
	if st == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	var req addStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	st.UpdateSchedule(req.StartAt, req.StopAt, req.Recurrence, req.AutoRemove, req.Contact)
	s.relay.PersistNow()
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
		if !s.relay.PortClaimed(p) && !s.relay.PortClaimed(p+s.egressOffset) {
			free = append(free, p)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(free)
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"host":           s.publicHost,
		"portLow":        s.portLow,
		"portHigh":       s.portHigh,
		"egressOffset":   s.egressOffset,
		"viewEnabled":    s.viewEnabled,
		"serverTimeZone": serverTimeZone,
		"idleRemoveMin":  s.relay.IdleRemoveMin(),
	})
}

// handleViewStreams is the public read-only status endpoint used by the
// infoscreen and DataMiner.
func (s *server) handleViewStreams(w http.ResponseWriter, r *http.Request) {
	streams := s.relay.ListStreams()
	sort.Slice(streams, func(i, j int) bool { return streams[i].InPort < streams[j].InPort })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(streams)
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, st := range s.relay.ListStreams() {
		labels := fmt.Sprintf(`stream="%s",contact="%s",in_port="%d",out_port="%d"`,
			st.Name, st.Contact, st.InPort, st.OutPort)
		up := 0
		if st.State == relay.StateRelaying {
			up = 1
		}
		fmt.Fprintf(w, "srt_stream_up{%s} %d\n", labels, up)
		fmt.Fprintf(w, "srt_stream_bitrate_kbps{%s} %g\n", labels, st.Stats.BitrateKbps)
		fmt.Fprintf(w, "srt_stream_bytes_in_total{%s} %d\n", labels, st.Stats.BytesIn)
		fmt.Fprintf(w, "srt_stream_bytes_out_total{%s} %d\n", labels, st.Stats.BytesOut)
		fmt.Fprintf(w, "srt_stream_retransmitted_total{%s} %d\n", labels, st.Stats.Retransmitted)
		fmt.Fprintf(w, "srt_stream_lost_total{%s} %d\n", labels, st.Stats.Lost)
		fmt.Fprintf(w, "srt_stream_jitter_ms{%s} %d\n", labels, st.Stats.JitterMs)
		fmt.Fprintf(w, "srt_stream_rtt_ms{%s} %g\n", labels, st.Stats.RTTMs)
	}
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
	mux.HandleFunc("/api/streams/", s.auth.requireAuth(s.handleStreamPath))
	mux.HandleFunc("/api/ports/free", s.auth.requireAuth(s.handleFreePorts))
	mux.Handle("/ws", s.auth.requireWS(s.handleWS))

	// Public read-only view (only when --view is enabled).
	if s.viewEnabled {
		mux.HandleFunc("/api/view/streams", s.handleViewStreams)
		mux.HandleFunc("/metrics", s.handleMetrics)
		mux.Handle("/ws/view", websocket.Handler(s.handleViewWS))
	}

	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)
	if s.viewEnabled {
		// Serve the same single-page UI for /view; the page renders read-only.
		mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, sub, "index.html")
		})
	}
	return mux
}

// handleStreamPath routes PATCH (edit) vs DELETE (remove) for /api/streams/{id}.
func (s *server) handleStreamPath(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		s.handlePatchStream(w, r)
		return
	}
	s.handleRemoveStream(w, r)
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

func startServer(r *relay.Relay, auth *authManager, addr string, portLow, portHigh, egressOffset int, publicHost string, viewEnabled bool) {
	s := newServer(r, auth, portLow, portHigh, egressOffset, publicHost, viewEnabled)
	log.Printf("web ui on http://%s", addr)
	if viewEnabled {
		log.Printf("read-only view enabled at http://%s/view", addr)
	}
	log.Fatal(http.ListenAndServe(addr, s.routes()))
}
