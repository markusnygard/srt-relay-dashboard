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
	mu           sync.Mutex
	clients      map[*websocket.Conn]bool
	portLow      int
	portHigh     int
	egressOffset int
	publicHost   string
}

func newServer(r *relay.Relay, portLow, portHigh, egressOffset int, publicHost string) *server {
	s := &server{
		relay:        r,
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

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/streams", s.handleListStreams)
	mux.HandleFunc("/api/streams/add", s.handleAddStream)
	mux.HandleFunc("/api/streams/", s.handleRemoveStream)
	mux.HandleFunc("/api/ports/free", s.handleFreePorts)
	mux.Handle("/ws", websocket.Handler(s.handleWS))

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

func startServer(r *relay.Relay, addr string, portLow, portHigh, egressOffset int, publicHost string) {
	s := newServer(r, portLow, portHigh, egressOffset, publicHost)
	log.Printf("web ui on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, s.routes()))
}
