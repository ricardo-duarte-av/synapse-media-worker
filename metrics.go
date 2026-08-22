package main

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics.go exposes the counters needed to tell whether the worker is
// actually taking load off Synapse, and where it is falling back.

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_requests_total",
		Help: "Media requests handled, by endpoint and response status.",
	}, []string{"endpoint", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "synapse_media_worker_request_duration_seconds",
		Help:    "Time to serve a media request.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"endpoint"})

	// thumbnailOutcome is the number that justifies the design: how often the
	// worker serves from Synapse's own thumbnails, from its cache, from a fresh
	// generation, or has to fall back to Synapse.
	thumbnailOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_thumbnail_outcome_total",
		Help: "Thumbnail requests by how they were satisfied.",
	}, []string{"outcome"})

	thumbnailGenerateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "synapse_media_worker_thumbnail_generate_seconds",
		Help:    "Time spent generating a thumbnail.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	})

	proxiedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_proxied_total",
		Help: "Requests handed back to Synapse, by reason.",
	}, []string{"endpoint", "reason"})

	authOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_auth_total",
		Help: "Access token validations by outcome.",
	}, []string{"outcome"})

	tokenCacheSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_media_worker_token_cache_entries",
		Help: "Access token verdicts currently cached.",
	})
)

// statusRecorder captures the response status for metrics and logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// ServeContent relies on for flushing.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ReadFrom forwards to the underlying writer's implementation, which is what
// lets net/http sendfile a media body straight from the file to the socket.
// Without this, wrapping the ResponseWriter for metrics would quietly demote
// every download to a userspace copy loop.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	var n int64
	var err error
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(s.ResponseWriter, r)
	}
	s.bytes += n
	return n, err
}

// instrument records request counts and latency for an endpoint.
func instrument(endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		annotate(r.Context(), func(rl *reqLog) { rl.endpoint = endpoint })
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		requestsTotal.WithLabelValues(endpoint, strconv.Itoa(rec.status)).Inc()
		requestDuration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	})
}
