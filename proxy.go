package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
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

// Proxy forwards a request to a Synapse worker.
type Proxy struct {
	proxy  *httputil.ReverseProxy
	target string
	log    zerolog.Logger
}

func NewProxy(t UpstreamTarget, log zerolog.Logger) (*Proxy, error) {
	if !t.configured() {
		return nil, fmt.Errorf("upstream has neither socket nor url")
	}

	transport := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}

	targetURL := t.URL
	description := t.URL
	if t.Socket != "" {
		socket := t.Socket
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		// The host is ignored once the dialer is pinned to a socket.
		targetURL = "http://synapse"
		description = "unix:" + socket
	}

	parsed, err := url.Parse(strings.TrimRight(targetURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing upstream url: %w", err)
	}

	p := &Proxy{target: description, log: log}
	p.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(parsed)
			// Preserve the original Host so Synapse's own routing and any
			// virtual-host logic behave as if it had been called directly.
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn().Err(err).
				Str("upstream", p.target).
				Str("path", r.URL.Path).
				Msg("Upstream request failed")
			writeMatrixError(w, http.StatusBadGateway, "M_UNKNOWN",
				"Media worker could not reach the homeserver")
		},
	}
	return p, nil
}

// ServeHTTP forwards the request upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}

// Target describes where this proxy points, for logging.
func (p *Proxy) Target() string { return p.target }
