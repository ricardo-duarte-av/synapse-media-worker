package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// proxy.go hands requests the worker cannot answer back to Synapse.
//
// The worker covers the common read paths; the awkward remainder — uncached
// remote media that needs a federated fetch, animated thumbnails, formats the
// worker will not decode — stays in Synapse, which already handles rate
// limiting, spam checks and storage for those cases. Proxying rather than
// reimplementing is what keeps the worker small.
//
// Several upstreams may be configured. Requests are spread by least
// connections rather than round robin because these responses vary enormously
// in cost: a cached thumbnail returns immediately, while an uncached remote
// download blocks on a federated fetch from another server. Round robin would
// keep handing work to a worker already blocked on a slow transfer.

// upstreamEndpoint is one Synapse worker, with its own connection pool.
type upstreamEndpoint struct {
	name      string
	base      *url.URL
	transport *http.Transport
	inflight  atomic.Int64
}

// Proxy forwards requests to one or more Synapse workers.
type Proxy struct {
	endpoints []*upstreamEndpoint
	proxy     *httputil.ReverseProxy
	log       zerolog.Logger
	rr        atomic.Uint64
}

// placeholderHost is the host the rewrite step sets. The real target is chosen
// per request in RoundTrip, once it is known which upstream is least busy.
var placeholderURL = &url.URL{Scheme: "http", Host: "upstream"}

func NewProxy(t UpstreamTarget, log zerolog.Logger) (*Proxy, error) {
	endpoints := t.Endpoints()
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("upstream has neither socket nor url")
	}

	p := &Proxy{log: log}
	for _, e := range endpoints {
		ep, err := newUpstreamEndpoint(e)
		if err != nil {
			return nil, err
		}
		p.endpoints = append(p.endpoints, ep)
	}

	p.proxy = &httputil.ReverseProxy{
		Transport: p,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(placeholderURL)
			// Preserve the original Host so Synapse's own routing and any
			// virtual-host logic behave as if it had been called directly.
			r.Out.Host = r.In.Host
			forwardXForwardedFor(r)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn().Err(err).
				Str("upstream", p.Target()).
				Str("path", r.URL.Path).
				Msg("Upstream request failed")
			writeMatrixError(w, http.StatusBadGateway, "M_UNKNOWN",
				"Media worker could not reach the homeserver")
		},
	}
	return p, nil
}

func newUpstreamEndpoint(e UpstreamEndpoint) (*upstreamEndpoint, error) {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		// No ResponseHeaderTimeout: an uncached remote download makes Synapse
		// fetch from another server first, and headers do not arrive until it
		// has. The client's own context bounds the wait.
	}

	base := e.URL
	if e.Socket != "" {
		socket := e.Socket
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		// The host is ignored once the dialer is pinned to a socket.
		base = "http://synapse"
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing upstream url %q: %w", base, err)
	}
	return &upstreamEndpoint{name: e.Name(), base: parsed, transport: transport}, nil
}

// pick returns the endpoint with the fewest requests in flight, breaking ties
// round robin so an idle pool still spreads load.
func (p *Proxy) pick(exclude map[*upstreamEndpoint]bool) *upstreamEndpoint {
	var best *upstreamEndpoint
	var bestLoad int64
	offset := p.rr.Add(1)
	n := uint64(len(p.endpoints))
	for i := range p.endpoints {
		ep := p.endpoints[(offset+uint64(i))%n]
		if exclude[ep] {
			continue
		}
		load := ep.inflight.Load()
		if best == nil || load < bestLoad {
			best, bestLoad = ep, load
		}
	}
	return best
}

// RoundTrip dispatches to the least busy upstream, retrying elsewhere if the
// connection could not be established.
//
// Retrying is safe only before any part of the response has been written, and
// only for requests with no body to replay. Both hold here: the worker proxies
// GET requests, and a dial failure means the upstream never saw the request.
func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	tried := make(map[*upstreamEndpoint]bool, len(p.endpoints))
	var lastErr error

	for range p.endpoints {
		ep := p.pick(tried)
		if ep == nil {
			break
		}
		tried[ep] = true

		out := r.Clone(r.Context())
		out.URL.Scheme = ep.base.Scheme
		out.URL.Host = ep.base.Host

		ep.inflight.Add(1)
		upstreamInflight.WithLabelValues(ep.name).Inc()

		resp, err := ep.transport.RoundTrip(out)
		if err != nil {
			ep.inflight.Add(-1)
			upstreamInflight.WithLabelValues(ep.name).Dec()
			upstreamRequests.WithLabelValues(ep.name, "error").Inc()
			lastErr = err
			if !isConnectError(err) {
				return nil, err
			}
			p.log.Warn().Err(err).Str("upstream", ep.name).
				Msg("Upstream unreachable, trying another")
			continue
		}

		upstreamRequests.WithLabelValues(ep.name, "ok").Inc()
		// The request is only finished once the body has been consumed, so the
		// in-flight count is released there rather than here. Without this the
		// balancer would see every streaming download as already complete.
		resp.Body = &releasingBody{
			ReadCloser: resp.Body,
			release: func() {
				ep.inflight.Add(-1)
				upstreamInflight.WithLabelValues(ep.name).Dec()
			},
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstream available")
	}
	return nil, lastErr
}

// isConnectError reports whether the request failed before the upstream could
// have acted on it, which is the only case where retrying elsewhere is safe.
func isConnectError(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	return opErr.Op == "dial"
}

// releasingBody runs release exactly once, when the body is closed or fully
// read, so an in-flight count tracks the real duration of a streamed response.
type releasingBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// ServeHTTP forwards the request upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}

// Target describes where this proxy points, for logging.
func (p *Proxy) Target() string {
	names := make([]string, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		names = append(names, ep.name)
	}
	return strings.Join(names, ", ")
}

// forwardXForwardedFor carries the X-Forwarded-* chain through to Synapse.
//
// Two things conspire here. ReverseProxy strips Forwarded and every
// X-Forwarded-* header from the outbound request whenever a Rewrite hook is
// set, leaving it to the hook to restore them. And ProxyRequest.SetXForwarded,
// the obvious way to do that, *deletes* X-Forwarded-For when the inbound peer
// address is not host:port -- which is exactly what a unix socket listener
// produces.
//
// Synapse needs that header: synapse/rest/client/media.py reads
// request.getClientAddress().host for rate limiting on the remote-media path,
// and on a unix socket with no X-Forwarded-For that is a UNIXAddress with no
// .host, so the request dies with
//
//	AttributeError: 'UNIXAddress' object has no attribute 'host'
//
// which surfaces as a 500 on every uncached remote media fetch. So the chain
// from the proxy in front is copied across explicitly, and our own peer is
// appended only when it is a real TCP address.
func forwardXForwardedFor(r *httputil.ProxyRequest) {
	prior := r.In.Header.Get("X-Forwarded-For")
	clientIP, _, err := net.SplitHostPort(r.In.RemoteAddr)
	switch {
	case err == nil && prior != "":
		r.Out.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	case err == nil:
		r.Out.Header.Set("X-Forwarded-For", clientIP)
	case prior != "":
		// Unix socket listener: we have no address of our own to add, but the
		// chain from the proxy in front must still reach Synapse.
		r.Out.Header.Set("X-Forwarded-For", prior)
	}

	// Preserve the front proxy's view of the original request where it gave
	// one, since it knows the real scheme and host and this worker does not.
	if host := r.In.Header.Get("X-Forwarded-Host"); host != "" {
		r.Out.Header.Set("X-Forwarded-Host", host)
	} else {
		r.Out.Header.Set("X-Forwarded-Host", r.In.Host)
	}
	if proto := r.In.Header.Get("X-Forwarded-Proto"); proto != "" {
		r.Out.Header.Set("X-Forwarded-Proto", proto)
	} else if r.In.TLS != nil {
		r.Out.Header.Set("X-Forwarded-Proto", "https")
	} else {
		r.Out.Header.Set("X-Forwarded-Proto", "http")
	}
}
