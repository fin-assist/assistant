// Package ops is the operational HTTP listener: /metrics for Prometheus, /healthz
// (the process is alive) and /readyz (it can serve: the database answers). It works the
// same under k3s and under systemd on the stand; gRPC health on the main port reports the
// same readiness.
package ops

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a registry with the Go and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Readiness keeps the last result of the readiness check. Once Drain is called the
// process is shutting down and stays not ready for good, whatever the check says.
// One mutex orders Drain against Watch's update and its onChange call, so no
// onChange(true) is in flight or starts after Drain returns.
type Readiness struct {
	mu       sync.Mutex
	ready    bool
	draining bool
	// beforeDrainLock, set only by tests, runs right before Drain takes mu: it marks
	// the point where Drain starts waiting for an in-flight notification.
	beforeDrainLock func()
}

// Ready reports the last result; false after Drain.
func (r *Readiness) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready && !r.draining
}

// Drain marks the process as shutting down: /readyz answers 503 from now on, so
// balancers stop sending new requests during SHUTDOWN_DELAY. It is irreversible and
// waits for an onChange that Watch has already started.
func (r *Readiness) Drain() {
	if r.beforeDrainLock != nil {
		r.beforeDrainLock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	r.ready = false
}

// Watch runs check every interval until ctx ends and calls onChange when the result flips.
// The check itself runs without the lock; the update and onChange run under it.
func (r *Readiness) Watch(ctx context.Context, interval time.Duration, log *slog.Logger, check func(context.Context) error, onChange func(bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for first := true; ; first = false {
		cctx, cancel := context.WithTimeout(ctx, interval)
		err := check(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		r.update(ctx, log, err, first, onChange)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Readiness) update(ctx context.Context, log *slog.Logger, err error, first bool, onChange func(bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return // readiness is down for good; gRPC health is left to Shutdown
	}
	ok := err == nil
	if r.ready == ok && !first {
		return
	}
	r.ready = ok
	if !ok {
		log.WarnContext(ctx, "not ready", "error", err)
	} else {
		log.InfoContext(ctx, "ready")
	}
	onChange(ok)
}

// Handler serves /metrics, /healthz and /readyz.
func Handler(reg *prometheus.Registry, r *Readiness) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Server returns the HTTP server with timeouts; it carries no personal data.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
}
