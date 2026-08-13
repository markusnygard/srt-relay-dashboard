package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"srt-relay-app/internal/relay"
)

func main() {
	httpAddr := flag.String("http", "0.0.0.0:3001", "web ui listen address")
	host := flag.String("host", "0.0.0.0", "SRT bind host")
	latency := flag.Int("latency", 1000, "SRT latency in ms")
	portLow := flag.Int("port-low", 21001, "lowest available ingress port")
	portHigh := flag.Int("port-high", 21100, "highest available ingress port")
	egressOffset := flag.Int("egress-offset", 100, "egress port = ingress + offset")
	portsFile := flag.String("ports-file", "", "path to a JSON file listing allowed ingress ports (overrides --port-low/--port-high)")
	publicHost := flag.String("host-ip", "", "IP advertised to senders/receivers in the web UI (auto-detected if empty)")
	usersFile := flag.String("users", "users.json", "path to the users file")
	streamsFile := flag.String("streams", "streams.json", "path to the streams/schedule file")
	idleRemoveMin := flag.Int("idle-remove-min", 0, "remove streams with no publisher for this many minutes (0 = disabled)")
	viewEnabled := flag.Bool("view", false, "enable the public read-only view page")
	flag.Parse()

	ip := *publicHost
	if ip == "" {
		ip = detectPublicIP()
	}
	if ip == "" {
		ip = "localhost"
	}
	log.Printf("advertising host for relay URLs: %s", ip)

	auth, err := newAuthManager(*usersFile)
	if err != nil {
		log.Fatalf("failed to load users: %v", err)
	}
	log.Printf("authentication enabled (users: %s)", *usersFile)

	log.Printf("srt-relay-app starting: http=%s srt host=%s latency=%dms ports=%d-%d egress+%d view=%v",
		*httpAddr, *host, *latency, *portLow, *portHigh, *egressOffset, *viewEnabled)

	// Optional explicit port list from a config file.
	var allowedPorts []int
	if *portsFile != "" {
		allowedPorts, err = loadPortList(*portsFile)
		if err != nil {
			log.Fatalf("failed to load ports file: %v", err)
		}
		log.Printf("using %d allowed ingress ports from %s", len(allowedPorts), *portsFile)
	}

	log.Printf("initializing SRT...")
	r := relay.New(*host, *latency, nil)
	log.Printf("SRT initialized")
	defer relay.CleanupSRT()

	r.ConfigurePersistence(*streamsFile, *idleRemoveMin)
	if err := r.LoadStreams(); err != nil {
		log.Printf("warning: failed to load streams: %v", err)
	}
	r.Start()

	loc := time.Now().Location()
	serverTimeZone = loc.String()

	// Start with no streams; streams are added via the web UI.
	log.Printf("starting web server on %s", *httpAddr)
	startServer(r, auth, *httpAddr, *portLow, *portHigh, *egressOffset, allowedPorts, ip, *viewEnabled)
}

// loadPortList reads a JSON file of allowed ingress port numbers.
func loadPortList(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ports []int
	if err := json.Unmarshal(data, &ports); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d in %s", p, path)
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate port %d in %s", p, path)
		}
		seen[p] = true
	}
	return ports, nil
}

// detectPublicIP finds a non-loopback IPv4 address of this host.
func detectPublicIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			ip := ipnet.IP.To4()
			if ip != nil && !strings.HasPrefix(ip.String(), "169.254.") {
				return ip.String()
			}
		}
	}
	return ""
}
