package main

import (
	"flag"
	"log"

	"srt-relay-app/internal/relay"
)

func main() {
	httpAddr := flag.String("http", "0.0.0.0:8080", "web ui listen address")
	host := flag.String("host", "0.0.0.0", "SRT bind host")
	latency := flag.Int("latency", 1000, "SRT latency in ms")
	portLow := flag.Int("port-low", 21001, "lowest available ingress port")
	portHigh := flag.Int("port-high", 21100, "highest available ingress port")
	egressOffset := flag.Int("egress-offset", 100, "egress port = ingress + offset")
	flag.Parse()

	r := relay.New(*host, *latency, nil)
	defer relay.CleanupSRT()

	// Start with no streams; streams are added via the web UI.
	log.Printf("srt-relay-app starting: http=%s srt host=%s latency=%dms ports=%d-%d egress+%d",
		*httpAddr, *host, *latency, *portLow, *portHigh, *egressOffset)

	startServer(r, *httpAddr, *portLow, *portHigh, *egressOffset)
}

