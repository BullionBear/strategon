package telemetry

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// MetricsHandler serves Prometheus text exposition of the latest snapshot.
// No client library — hand-written text format (zero dependency).
func (c *Collector) MetricsHandler(machineID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s := c.Latest()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if s == nil {
			return
		}
		var b strings.Builder
		m := machineID
		if res := s.Resources; res != nil {
			writeGauge(&b, "strategon_machine_cpu_percent", "Host CPU utilization percent.",
				fmt.Sprintf("strategon_machine_cpu_percent{machine=%q} %g\n", m, res.GetCpuPercent()))
			writeGauge(&b, "strategon_machine_mem_bytes", "Host memory used bytes.",
				fmt.Sprintf("strategon_machine_mem_bytes{machine=%q} %d\n", m, res.GetMemoryUsedBytes()))
			writeGauge(&b, "strategon_machine_mem_total_bytes", "Host memory total bytes.",
				fmt.Sprintf("strategon_machine_mem_total_bytes{machine=%q} %d\n", m, res.GetMemoryTotalBytes()))
			writeGauge(&b, "strategon_machine_load1", "Host load average (1m).",
				fmt.Sprintf("strategon_machine_load1{machine=%q} %g\n", m, res.GetLoad1()))
		}
		procs := append([]*pb.ProcessMetrics(nil), s.Processes...)
		sort.Slice(procs, func(i, j int) bool { return procs[i].GetStrategy() < procs[j].GetStrategy() })
		if len(procs) > 0 {
			fmt.Fprintf(&b, "# HELP strategon_process_cpu_percent Strategy process CPU percent.\n")
			fmt.Fprintf(&b, "# TYPE strategon_process_cpu_percent gauge\n")
			for _, p := range procs {
				fmt.Fprintf(&b, "strategon_process_cpu_percent{machine=%q,strategy=%q} %g\n",
					m, p.GetStrategy(), p.GetCpuPercent())
			}
			fmt.Fprintf(&b, "# HELP strategon_process_rss_bytes Strategy process RSS bytes.\n")
			fmt.Fprintf(&b, "# TYPE strategon_process_rss_bytes gauge\n")
			for _, p := range procs {
				fmt.Fprintf(&b, "strategon_process_rss_bytes{machine=%q,strategy=%q} %d\n",
					m, p.GetStrategy(), p.GetRssBytes())
			}
			fmt.Fprintf(&b, "# HELP strategon_process_restart_total Strategy process restart count.\n")
			fmt.Fprintf(&b, "# TYPE strategon_process_restart_total counter\n")
			for _, p := range procs {
				fmt.Fprintf(&b, "strategon_process_restart_total{machine=%q,strategy=%q} %d\n",
					m, p.GetStrategy(), p.GetRestartCount())
			}
			fmt.Fprintf(&b, "# HELP strategon_process_alive Strategy process liveness (1=alive).\n")
			fmt.Fprintf(&b, "# TYPE strategon_process_alive gauge\n")
			for _, p := range procs {
				alive := 0
				if p.GetAlive() {
					alive = 1
				}
				fmt.Fprintf(&b, "strategon_process_alive{machine=%q,strategy=%q} %d\n",
					m, p.GetStrategy(), alive)
			}
		}
		_, _ = w.Write([]byte(b.String()))
	})
}

func writeGauge(b *strings.Builder, name, help, line string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	b.WriteString(line)
}

// ListenAndServeMetrics starts a plain HTTP server exposing /metrics.
func ListenAndServeMetrics(addr, machineID string, c *Collector) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", c.MetricsHandler(machineID))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}
