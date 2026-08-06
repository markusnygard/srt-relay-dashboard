package main

import (
	"flag"
	"log"
	"net"
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
	startServer(r, auth, *httpAddr, *portLow, *portHigh, *egressOffset, ip, *viewEnabled)
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
