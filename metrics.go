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

	responseBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_response_bytes_total",
		Help: "Response body bytes written, by endpoint.",
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

	// Per-upstream counters, so an unbalanced pool or one sick worker is
	// visible rather than showing up only as latency.
	upstreamInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "synapse_media_worker_upstream_inflight",
		Help: "Proxied requests currently in flight, by upstream.",
	}, []string{"upstream"})

	upstreamRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_upstream_requests_total",
		Help: "Proxied requests dispatched, by upstream and result.",
	}, []string{"upstream", "result"})

	// remoteFetches counts federated downloads the worker performed itself,
	// which is the measure of how much work is no longer reaching Synapse.
	remoteFetches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_remote_fetches_total",
		Help: "Remote media the worker fetched over federation, by result.",
	}, []string{"result"})

	// thumbnailWriteThrough tracks thumbnails written into Synapse's own store
	// rather than the worker's cache.
	thumbnailWriteThrough = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_thumbnail_write_through_total",
		Help: "Generated remote thumbnails written into Synapse's media store, by result.",
	}, []string{"result"})

	// uploadsTotal counts upload requests by which endpoint served them and
	// how they ended. The two are separate dimensions: "async" is an endpoint,
	// not an outcome.
	uploadsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "synapse_media_worker_uploads_total",
		Help: "Upload requests handled by the worker, by endpoint and result.",
	}, []string{"endpoint", "result"})

	uploadedBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "synapse_media_worker_uploaded_bytes_total",
		Help: "Bytes accepted from local users and written to the media store.",
	})

	// pendingMediaReserved tracks async media IDs created but not yet
	// uploaded to, which is what max_pending_media_uploads bounds.
	pendingMediaReserved = promauto.NewCounter(prometheus.CounterOpts{
		Name: "synapse_media_worker_pending_media_reserved_total",
		Help: "Async media IDs reserved by /_matrix/media/v1/create.",
	})

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
		responseBytes.WithLabelValues(endpoint).Add(float64(rec.bytes))
	})
}
