// netem is the link-impairment proxy used by the benchmark matrix. It
// forwards one port to a real backend and lets a script degrade the link over
// HTTP, so the same cloudfs mount can be measured on a LAN, a WAN and a bad
// WAN without touching the network stack.
//
//	netem --listen 127.0.0.1:2222 --target 192.168.0.20:22 --control 127.0.0.1:2223 \
//	      [--rtt 50ms] [--jitter 5ms] [--loss 0.01] [--bandwidth 20mbit]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloudfs/internal/netem"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:2222", "address to accept connections on")
	target := flag.String("target", "", "backend host:port to forward to")
	control := flag.String("control", "127.0.0.1:2223", "HTTP control address")
	rtt := flag.Duration("rtt", 0, "added round-trip time (split evenly per direction)")
	jitter := flag.Duration("jitter", 0, "latency jitter per direction")
	loss := flag.Float64("loss", 0, "per-chunk stall probability, modelling packet loss")
	stall := flag.Duration("stall", 200*time.Millisecond, "how long a lost chunk stalls the stream")
	reset := flag.Float64("reset", 0, "per-chunk probability of tearing the connection down")
	bandwidth := flag.String("bandwidth", "", "cap per direction, e.g. 20mbit")
	flag.Parse()
	if *target == "" {
		fmt.Fprintln(os.Stderr, "netem: --target is required")
		os.Exit(2)
	}
	cfg := netem.Config{Latency: *rtt / 2, Jitter: *jitter, StallProb: *loss, StallFor: *stall, ResetProb: *reset}
	if *bandwidth != "" {
		bps, err := netem.ParseBandwidth(*bandwidth)
		if err != nil {
			fmt.Fprintln(os.Stderr, "netem:", err)
			os.Exit(2)
		}
		cfg.BandwidthBps = bps
	}
	p, err := netem.Listen(*listen, *target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netem:", err)
		os.Exit(1)
	}
	p.Set(cfg)
	fmt.Printf("netem: %s -> %s, control on http://%s (rtt %s, jitter %s, loss %.3f, reset %.3f, bandwidth %s)\n",
		p.Addr(), *target, *control, *rtt, *jitter, *loss, *reset, valueOr(*bandwidth, "unlimited"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := p.Serve(ctx, *control); err != nil {
		fmt.Fprintln(os.Stderr, "netem:", err)
		os.Exit(1)
	}
	p.Close()
}

func valueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
