package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"maunium.net/go/mautrix/federation"
)

// Server name resolution for outbound federation requests.
//
// mautrix's ServerResolvingTransport resolves the whole chain (.well-known,
// SRV, fallback) once and caches the outcome for 24 hours, whatever happened.
// A .well-known server that was restarting, or a DNS lookup that timed out,
// is not treated as an error: it silently becomes "hostname:8448", and every
// request to that origin goes to the wrong place for a day.
//
// This reproduces Synapse instead (synapse/http/federation/
// well_known_resolver.py, srv_resolver.py and matrix_federation_agent.py at
// v1.164.1). The two halves are resolved at different times, as there:
//
//   - .well-known per request, because it decides the Host header. Results are
//     cached for the server's Cache-Control/Expires (clamped to 5m..48h,
//     default 24h ±10%). A failure is cached for 1h, or for only 2m if the
//     domain had a valid .well-known recently, and in that case the fetch is
//     retried three times first. A temporary failure (no connection, 5xx)
//     while a result is still cached keeps the cached result: the refetch
//     starts at 80% of its TTL, so a short outage is never noticed.
//   - SRV per connection, at dial time. NXDOMAIN or no records means
//     hostname:8448. Any other DNS failure reuses the last answer, or fails
//     the connection when there is none. It never falls back to 8448, which
//     would send the request to a host that is not the server.
//
// Deviation: Go's resolver does not expose record TTLs, so SRV answers are not
// cached for their TTL; every new connection asks again. The connection pool
// makes that rare, and the last good answer is kept for DNS failures as
// Synapse keeps it.

const (
	wellKnownDefaultCachePeriod = 24 * time.Hour
	wellKnownCacheJitter        = 0.1
	wellKnownInvalidCachePeriod = 1 * time.Hour
	wellKnownDownCachePeriod    = 2 * time.Minute
	wellKnownRememberHadValid   = 2 * time.Hour
	wellKnownMaxCachePeriod     = 48 * time.Hour
	wellKnownMinCachePeriod     = 5 * time.Minute
	wellKnownMaxSize            = 50 * 1024
	wellKnownGracePeriodFactor  = 0.2
	wellKnownRetryAttempts      = 3
	wellKnownRetryDelay         = 500 * time.Millisecond
)

// wellKnownEntry is a cached .well-known outcome. delegated is "" for "no
// delegation", which is also what a failure caches.
type wellKnownEntry struct {
	delegated string
	expires   time.Time
	ttl       time.Duration
}

// srvTarget is one address to try, in order.
type srvTarget struct {
	host string
	port uint16
}

// dialTarget is what a request resolved to before DNS: a hostname with an
// explicit port (or an IP literal) is dialled as is; otherwise SRV decides.
type dialTarget struct {
	host     string
	port     uint16
	explicit bool
}

type dialTargetKey struct{}

// errWellKnownFailure carries whether the failure may be temporary, which
// decides whether a still-cached result survives it.
type errWellKnownFailure struct {
	temporary bool
	err       error
}

func (e *errWellKnownFailure) Error() string { return e.err.Error() }
func (e *errWellKnownFailure) Unwrap() error { return e.err }

// FederationTransport is an http.RoundTripper for matrix-federation:// URLs.
type FederationTransport struct {
	// WellKnownClient fetches .well-known. It should carry the IP filter and
	// the User-Agent, as Synapse's well-known agent does.
	WellKnownClient *http.Client
	// Transport sends the request once resolved. Its DialContext is ours.
	Transport *http.Transport

	dial   exhttp.DialerFunc
	lookup func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	log    zerolog.Logger

	// Seams for tests.
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64

	mu        sync.Mutex
	wellKnown map[string]wellKnownEntry
	hadValid  map[string]time.Time
	srv       map[string][]srvTarget
	inflight  map[string]chan struct{}
	pinned    map[string]string
}

var _ http.RoundTripper = (*FederationTransport)(nil)

// InstallFederationTransport replaces the client's resolving transport with
// one that resolves server names as Synapse does. Call it before
// InstallUserAgent, which wraps whatever transport is in place.
func InstallFederationTransport(client *federation.Client, settings exhttp.ClientSettings, log zerolog.Logger) *FederationTransport {
	ft := &FederationTransport{
		WellKnownClient: client.ExtHTTP,
		dial:            client.Dialer.DialContext,
		lookup:          net.DefaultResolver.LookupSRV,
		log:             log,
		now:             time.Now,
		sleep:           sleepCtx,
		jitter: func() float64 {
			return 1 - wellKnownCacheJitter + rand.Float64()*2*wellKnownCacheJitter
		},
		wellKnown: make(map[string]wellKnownEntry),
		hadValid:  make(map[string]time.Time),
		srv:       make(map[string][]srvTarget),
		inflight:  make(map[string]chan struct{}),
	}
	ft.Transport = settings.WithDial(ft.dialContext).Configure(&http.Transport{})
	client.HTTP.Transport = ft
	return ft
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pin makes serverName dial addr directly, bypassing discovery. Tests only.
func (ft *FederationTransport) pin(serverName, addr string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.pinned == nil {
		ft.pinned = make(map[string]string)
	}
	ft.pinned[serverName] = addr
}

func (ft *FederationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "matrix-federation" {
		return nil, fmt.Errorf("unsupported scheme: %s", req.URL.Scheme)
	}
	hostHeader, target, err := ft.resolve(req.Context(), req.URL.Host)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("failed to resolve server name %q: %w", req.URL.Host, err)
	}
	req = req.Clone(context.WithValue(req.Context(), dialTargetKey{}, target))
	req.URL.Scheme = "https"
	req.URL.Host = hostHeader
	req.Host = hostHeader
	return ft.Transport.RoundTrip(req)
}

// resolve applies .well-known delegation: it returns the Host header to send
// and where to connect, leaving SRV to dial time.
func (ft *FederationTransport) resolve(ctx context.Context, serverName string) (string, dialTarget, error) {
	ft.mu.Lock()
	addr, pinned := ft.pinned[serverName]
	ft.mu.Unlock()
	if pinned {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return "", dialTarget{}, err
		}
		port, _ := strconv.ParseUint(portStr, 10, 16)
		return serverName, dialTarget{host: host, port: uint16(port), explicit: true}, nil
	}

	host, port, ok := federation.ParseServerName(serverName)
	if !ok {
		return "", dialTarget{}, federation.ErrInvalidServerName
	}
	if port != 0 || net.ParseIP(host) != nil {
		return serverName, explicitTarget(host, port), nil
	}

	delegated := ft.getWellKnown(ctx, host)
	if delegated == "" {
		return serverName, dialTarget{host: host}, nil
	}
	dHost, dPort, ok := federation.ParseServerName(delegated)
	if !ok {
		// Synapse rewrites the URL to whatever m.server says and fails to
		// connect; failing here is the same outcome, without a guess.
		return "", dialTarget{}, fmt.Errorf("invalid m.server %q in .well-known of %s", delegated, host)
	}
	if dPort != 0 || net.ParseIP(dHost) != nil {
		return delegated, explicitTarget(dHost, dPort), nil
	}
	return delegated, dialTarget{host: dHost}, nil
}

func explicitTarget(host string, port uint16) dialTarget {
	if port == 0 {
		port = 8448
	}
	return dialTarget{host: host, port: port, explicit: true}
}

// getWellKnown returns the delegated server name for host, or "" for none.
// It never fails: a failure is "no delegation", cached briefly.
func (ft *FederationTransport) getWellKnown(ctx context.Context, host string) string {
	if delegated, fresh := ft.cachedWellKnown(host); fresh {
		return delegated
	}

	// Synapse does not linearise this, but a dead .well-known with retries
	// would otherwise be fetched once per concurrent request.
	release, err := ft.acquire(ctx, host)
	if err != nil {
		delegated, _ := ft.cachedWellKnown(host)
		return delegated
	}
	defer release()
	// Someone else may have just refreshed it.
	if delegated, fresh := ft.cachedWellKnown(host); fresh {
		return delegated
	}

	ft.mu.Lock()
	prev, hasPrev := ft.wellKnown[host]
	if hasPrev && !ft.now().Before(prev.expires) {
		hasPrev = false
	}
	ft.mu.Unlock()

	result, period, err := ft.fetchWellKnown(ctx, host)
	if err != nil {
		var f *errWellKnownFailure
		temporary := errors.As(err, &f) && f.temporary
		if hasPrev && prev.delegated != "" && temporary {
			ft.log.Info().Err(err).Str("server_name", host).Str("delegated", prev.delegated).
				Msg("Could not refresh .well-known; keeping the cached delegation")
			return prev.delegated
		}
		result = ""
		period = wellKnownInvalidCachePeriod
		if ft.hadValidWellKnown(host) {
			period = wellKnownDownCachePeriod
		}
		period = time.Duration(float64(period) * ft.jitter())
		ft.log.Info().Err(err).Str("server_name", host).Dur("cache_for", period).
			Msg("No usable .well-known; using the server name directly")
	}
	if period > 0 {
		ft.mu.Lock()
		ft.wellKnown[host] = wellKnownEntry{delegated: result, expires: ft.now().Add(period), ttl: period}
		ft.mu.Unlock()
	}
	return result
}

// cachedWellKnown returns the cached delegation and whether it is still
// before its refresh point (80% of its TTL).
func (ft *FederationTransport) cachedWellKnown(host string) (string, bool) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	e, ok := ft.wellKnown[host]
	if !ok {
		return "", false
	}
	now := ft.now()
	if !now.Before(e.expires) {
		return "", false
	}
	refreshAt := e.expires.Add(-time.Duration(wellKnownGracePeriodFactor * float64(e.ttl)))
	return e.delegated, now.Before(refreshAt)
}

func (ft *FederationTransport) hadValidWellKnown(host string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	until, ok := ft.hadValid[host]
	return ok && ft.now().Before(until)
}

// acquire serialises .well-known fetches per host, giving up with ctx.
func (ft *FederationTransport) acquire(ctx context.Context, host string) (func(), error) {
	for {
		ft.mu.Lock()
		ch, busy := ft.inflight[host]
		if !busy {
			ch = make(chan struct{})
			ft.inflight[host] = ch
			ft.mu.Unlock()
			return func() {
				ft.mu.Lock()
				delete(ft.inflight, host)
				ft.mu.Unlock()
				close(ch)
			}, nil
		}
		ft.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// fetchWellKnown fetches and parses host's .well-known, returning the
// delegated server ("" if m.server is empty) and how long to cache it.
func (ft *FederationTransport) fetchWellKnown(ctx context.Context, host string) (string, time.Duration, error) {
	resp, body, err := ft.requestWellKnown(ctx, host, ft.hadValidWellKnown(host))
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, &errWellKnownFailure{err: fmt.Errorf("non-200 response %d", resp.StatusCode)}
	}
	var parsed struct {
		Server *string `json:"m.server"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, &errWellKnownFailure{err: fmt.Errorf("invalid .well-known: %w", err)}
	}
	if parsed.Server == nil {
		return "", 0, &errWellKnownFailure{err: errors.New("no m.server in .well-known")}
	}
	result := *parsed.Server

	period, ok := cachePeriodFromHeaders(resp.Header, ft.now())
	if !ok {
		period = time.Duration(float64(wellKnownDefaultCachePeriod) * ft.jitter())
	} else {
		period = min(period, wellKnownMaxCachePeriod)
		period = max(period, wellKnownMinCachePeriod)
	}

	ft.mu.Lock()
	if result != "" {
		ft.hadValid[host] = ft.now().Add(period + wellKnownRememberHadValid)
	} else {
		delete(ft.hadValid, host)
	}
	ft.mu.Unlock()
	return result, period, nil
}

// requestWellKnown makes the request, retrying connection failures and 5xx
// when retry is set. Any other status is returned for the caller to judge.
func (ft *FederationTransport) requestWellKnown(ctx context.Context, host string, retry bool) (*http.Response, []byte, error) {
	uri := "https://" + host + "/.well-known/matrix/server"
	for attempt := 1; ; attempt++ {
		resp, body, err := ft.requestWellKnownOnce(ctx, uri)
		if err == nil {
			return resp, body, nil
		}
		var f *errWellKnownFailure
		if errors.As(err, &f) || ctx.Err() != nil {
			// Too large: temporary, but not worth retrying. Cancelled: done.
			return nil, nil, &errWellKnownFailure{temporary: true, err: err}
		}
		if !retry || attempt >= wellKnownRetryAttempts {
			return nil, nil, &errWellKnownFailure{temporary: true, err: err}
		}
		ft.log.Debug().Err(err).Str("url", uri).Msg("Fetching .well-known failed; retrying")
		if err := ft.sleep(ctx, wellKnownRetryDelay); err != nil {
			return nil, nil, &errWellKnownFailure{temporary: true, err: err}
		}
	}
}

func (ft *FederationTransport) requestWellKnownOnce(ctx context.Context, uri string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := ft.WellKnownClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, wellKnownMaxSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > wellKnownMaxSize {
		return nil, nil, &errWellKnownFailure{temporary: true,
			err: fmt.Errorf(".well-known is larger than %d bytes", wellKnownMaxSize)}
	}
	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return nil, nil, fmt.Errorf("non-200 response %d", resp.StatusCode)
	}
	return resp, body, nil
}

// cachePeriodFromHeaders is Synapse's _cache_period_from_headers: ok is false
// when the headers say nothing. no-store yields 0, which the caller clamps to
// the minimum like any other value.
func cachePeriodFromHeaders(h http.Header, now time.Time) (time.Duration, bool) {
	directives := map[string]string{}
	present := map[string]bool{}
	for _, hdr := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(hdr, ",") {
			k, v, _ := strings.Cut(directive, "=")
			k = strings.ToLower(strings.TrimSpace(k))
			directives[k] = strings.TrimSpace(v)
			present[k] = true
		}
	}
	if present["no-store"] {
		return 0, true
	}
	if v := directives["max-age"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second, true
		}
	}
	if expires := h.Values("Expires"); len(expires) > 0 {
		t, err := http.ParseTime(expires[len(expires)-1])
		if err != nil {
			return 0, true
		}
		return t.Sub(now), true
	}
	return 0, false
}

// dialContext connects to the request's target, resolving SRV if the target
// has no explicit port, and trying each address in order as Synapse does.
func (ft *FederationTransport) dialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	target, ok := ctx.Value(dialTargetKey{}).(dialTarget)
	if !ok {
		return nil, errors.New("no federation dial target in context")
	}
	var servers []srvTarget
	if target.explicit {
		servers = []srvTarget{{host: target.host, port: target.port}}
	} else {
		var err error
		if servers, err = ft.resolveSRV(ctx, target.host); err != nil {
			return nil, err
		}
	}
	var firstErr error
	for _, s := range servers {
		conn, err := ft.dial(ctx, network, net.JoinHostPort(s.host, strconv.Itoa(int(s.port))))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// resolveSRV is matrix_federation_agent's _resolve_server: _matrix-fed, then
// the legacy _matrix, then host:8448.
func (ft *FederationTransport) resolveSRV(ctx context.Context, host string) ([]srvTarget, error) {
	for _, service := range []string{"matrix-fed", "matrix"} {
		servers, err := ft.lookupService(ctx, service, host)
		if err != nil {
			return nil, err
		}
		if len(servers) > 0 {
			return servers, nil
		}
	}
	return []srvTarget{{host: host, port: 8448}}, nil
}

// lookupService is SrvResolver.resolve_service: nothing found is an empty
// list, and any other failure falls back to the last answer or fails.
func (ft *FederationTransport) lookupService(ctx context.Context, service, host string) ([]srvTarget, error) {
	name := "_" + service + "._tcp." + host
	_, records, err := ft.lookup(ctx, service, "tcp", host)
	if err != nil && len(records) == 0 {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, nil
		}
		ft.mu.Lock()
		cached := ft.srv[name]
		ft.mu.Unlock()
		if len(cached) > 0 {
			ft.log.Warn().Err(err).Str("record", name).Msg("SRV lookup failed; using the last answer")
			return cached, nil
		}
		return nil, fmt.Errorf("SRV lookup for %s: %w", name, err)
	}
	if len(records) == 1 && (records[0].Target == "." || records[0].Target == "") {
		return nil, fmt.Errorf("service %s unavailable", name)
	}
	servers := make([]srvTarget, 0, len(records))
	for _, r := range records {
		servers = append(servers, srvTarget{host: strings.TrimSuffix(r.Target, "."), port: r.Port})
	}
	ft.mu.Lock()
	ft.srv[name] = servers
	ft.mu.Unlock()
	return servers, nil
}
