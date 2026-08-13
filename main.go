package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
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
	portsFile := flag.String("ports-file", "", "path to a text file listing ingress-egress port pairs, one per line or comma-separated (overrides --port-low/--port-high/--egress-offset)")
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

	// Optional explicit port pairs from a config file.
	var portPairs []portPair
	if *portsFile != "" {
		portPairs, err = loadPortPairs(*portsFile)
		if err != nil {
			log.Fatalf("failed to load ports file: %v", err)
		}
		log.Printf("using %d port pairs from %s", len(portPairs), *portsFile)
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
	startServer(r, auth, *httpAddr, *portLow, *portHigh, *egressOffset, portPairs, ip, *viewEnabled)
}

// loadPortPairs reads a text file of ingress-egress port pairs. Entries may be
// separated by newlines or commas, e.g. "23001-23101, 23002-23102".
func loadPortPairs(path string) ([]portPair, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pairs []portPair
	seen := map[int]bool{}
	for _, tok := range strings.FieldsFunc(string(data), func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}) {
		parts := strings.Split(tok, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid entry %q in %s (want ingress-egress)", tok, path)
		}
		in, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		out, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid port in %q in %s", tok, path)
		}
		if in < 1 || in > 65535 || out < 1 || out > 65535 {
			return nil, fmt.Errorf("invalid port in %q in %s", tok, path)
		}
		if seen[in] {
			return nil, fmt.Errorf("duplicate ingress port %d in %s", in, path)
		}
		seen[in] = true
		pairs = append(pairs, portPair{In: in, Out: out})
	}
	return pairs, nil
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
