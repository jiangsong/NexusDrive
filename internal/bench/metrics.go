package bench

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// metricsSnapshot is the subset of the daemon's counters a workload is judged
// by. Zero values are returned when there is no daemon to ask, so a run
// against sshfs or a local disk simply reports no backend cost.
type metricsSnapshot struct {
	ok    bool
	calls map[string]int64
	// seconds is the time the daemon spent inside those calls, by operation.
	seconds    map[string]float64
	hits       int64
	misses     int64
	readBytes  int64
	writeBytes int64
	// Kernel-side request counters, when the daemon serves a FUSE mount.
	fuseOps       map[string]int64
	fuseReadBytes int64
	fuseReadSizes map[string]int64
}

type metricsDelta struct {
	calls       int64
	byOp        map[string]int64
	secondsByOp map[string]float64
	hits        int64
	misses      int64
	readBytes   int64
	writeBytes  int64

	fuseOps       map[string]int64
	fuseReadBytes int64
	fuseReadSizes map[string]int64
}

func (a metricsSnapshot) sub(b metricsSnapshot) metricsDelta {
	if !a.ok || !b.ok {
		return metricsDelta{}
	}
	d := metricsDelta{byOp: map[string]int64{}, secondsByOp: map[string]float64{}}
	for op, v := range a.calls {
		if n := v - b.calls[op]; n != 0 {
			d.byOp[op] = n
			d.calls += n
		}
	}
	for op, v := range a.seconds {
		if n := v - b.seconds[op]; n != 0 {
			d.secondsByOp[op] = n
		}
	}
	d.hits = a.hits - b.hits
	d.misses = a.misses - b.misses
	d.readBytes = a.readBytes - b.readBytes
	d.writeBytes = a.writeBytes - b.writeBytes
	d.fuseOps = diffMap(a.fuseOps, b.fuseOps)
	d.fuseReadBytes = a.fuseReadBytes - b.fuseReadBytes
	d.fuseReadSizes = diffMap(a.fuseReadSizes, b.fuseReadSizes)
	return d
}

// diffMap returns a-b per key, dropping zeros.
func diffMap(a, b map[string]int64) map[string]int64 {
	if len(a) == 0 {
		return nil
	}
	d := map[string]int64{}
	for k, v := range a {
		if n := v - b[k]; n != 0 {
			d[k] = n
		}
	}
	return d
}

// scrape reads the daemon's /metrics. Any failure yields a snapshot with
// ok=false so the run continues without counts rather than aborting.
func scrape(addr string) metricsSnapshot {
	if addr == "" {
		return metricsSnapshot{}
	}
	url := addr
	if !strings.HasPrefix(url, "http") {
		url = "http://" + url
	}
	url = strings.TrimSuffix(url, "/") + "/metrics"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return metricsSnapshot{}
	}
	defer resp.Body.Close()
	return parseMetrics(bufio.NewScanner(resp.Body))
}

// parseMetrics understands the Prometheus text format well enough for the
// handful of series the daemon exports.
func parseMetrics(sc *bufio.Scanner) metricsSnapshot {
	s := metricsSnapshot{ok: true, calls: map[string]int64{}, seconds: map[string]float64{},
		fuseOps: map[string]int64{}, fuseReadSizes: map[string]int64{}}
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		name, valStr := line[:sp], line[sp+1:]
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(name, "cloudfs_remote_calls_total{"):
			op := label(name, "op")
			if op != "" && op != "_all" {
				s.calls[op] += int64(val)
			}
		case strings.HasPrefix(name, "cloudfs_remote_seconds_total{"):
			if op := label(name, "op"); op != "" {
				s.seconds[op] += val
			}
		case strings.HasPrefix(name, "cloudfs_cache_hits_total"):
			s.hits = int64(val)
		case strings.HasPrefix(name, "cloudfs_cache_misses_total"):
			s.misses = int64(val)
		case strings.HasPrefix(name, "cloudfs_remote_read_bytes_total"):
			s.readBytes += int64(val)
		case strings.HasPrefix(name, "cloudfs_remote_write_bytes_total"):
			s.writeBytes += int64(val)
		case strings.HasPrefix(name, "cloudfs_fuse_ops_total{"):
			if op := label(name, "op"); op != "" {
				s.fuseOps[op] = int64(val)
			}
		case strings.HasPrefix(name, "cloudfs_fuse_read_bytes_total"):
			s.fuseReadBytes = int64(val)
		case strings.HasPrefix(name, "cloudfs_fuse_read_size_bucket{"):
			if le := label(name, "le"); le != "" {
				s.fuseReadSizes[le] = int64(val)
			}
		}
	}
	return s
}

func label(series, key string) string {
	i := strings.Index(series, key+"=\"")
	if i < 0 {
		return ""
	}
	rest := series[i+len(key)+2:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// DropCaches asks a daemon to start cold: POST /cache/drop on its control
// address. It is the Cold hook `cloudfs bench --cold` installs.
func DropCaches(addr string) error {
	url := addr
	if !strings.HasPrefix(url, "http") {
		url = "http://" + url
	}
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(url, "/")+"/cache/drop", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The control plane refuses a mutation without this header so a browser
	// form cannot reach it; a native client sends it.
	req.Header.Set("X-CloudFS-Control", "1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.Status}
	}
	return nil
}

type httpError struct{ status string }

func (e *httpError) Error() string { return "bench: cache drop failed: " + e.status }
