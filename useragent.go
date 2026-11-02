package main

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/federation"
)

// useragent.go makes the worker's outbound requests carry the User-Agent
// Synapse's would.
//
// mautrix sends "mautrix-go/<version> go/<version>" on federation requests and
// nothing at all (so Go's default) on .well-known lookups and media redirects.
// Peers see this worker as part of a Synapse homeserver, so it should look
// like one: remote admins filter, rate-limit and debug by User-Agent.
//
// Synapse builds three variants (synapse/server.py, http/client.py,
// http/matrixfederationclient.py, http/federation/matrix_federation_agent.py):
//
//   - signed federation requests: exactly "Synapse/<version>". The header is
//     set per request from hs.version_string, so the agent's suffixed default
//     never applies -- user_agent_suffix does not reach these.
//   - .well-known lookups: "Synapse/<version> <user_agent_suffix>", from the
//     MatrixFederationAgent's own user_agent.
//   - plain HTTPS such as a media download's Location redirect, which goes
//     through SimpleHttpClient: also "Synapse/<version> <user_agent_suffix>".
//
// <version> is SYNAPSE_VERSION, which is also what Synapse reports from
// /_matrix/federation/v1/version. So the worker asks its own homeserver, and
// keeps asking: Synapse is upgraded underneath a running worker.

// synapseUserAgentFallback is sent until the version is known. It is still
// recognisably Synapse, which is closer than mautrix's.
const synapseUserAgentFallback = "Synapse"

// UserAgent holds the User-Agent strings for outbound requests. Safe for
// concurrent use; the version can change while requests are in flight.
type UserAgent struct {
	base   atomic.Pointer[string]
	suffix string
	// fixed means base was configured and discovery must not replace it.
	fixed bool
}

// NewUserAgent returns a UserAgent. A non-empty configured value is used
// verbatim for the version part and never rediscovered.
func NewUserAgent(configured, suffix string) *UserAgent {
	ua := &UserAgent{suffix: suffix, fixed: configured != ""}
	base := synapseUserAgentFallback
	if configured != "" {
		base = configured
	}
	ua.base.Store(&base)
	return ua
}

// Federation is the User-Agent for signed federation requests.
func (u *UserAgent) Federation() string { return *u.base.Load() }

// External is the User-Agent for .well-known lookups and plain HTTPS fetches,
// which carry user_agent_suffix.
func (u *UserAgent) External() string {
	if u.suffix == "" {
		return u.Federation()
	}
	return u.Federation() + " " + u.suffix
}

func (u *UserAgent) set(base string) (changed bool) {
	old := u.base.Swap(&base)
	return *old != base
}

// userAgentTransport overrides the User-Agent on every request it carries.
type userAgentTransport struct {
	next http.RoundTripper
	ua   func() string
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it was given.
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", t.ua())
	return t.next.RoundTrip(req)
}

func withUserAgent(next http.RoundTripper, ua func() string) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &userAgentTransport{next: next, ua: ua}
}

// InstallUserAgent makes every request the federation client sends carry ua.
// It must run before the client is used: it replaces transports.
func InstallUserAgent(client *federation.Client, ua *UserAgent) {
	// .well-known resolution. Left unset, mautrix uses http.DefaultClient,
	// which sends Go's User-Agent and has no timeout. ExtHTTP's transport has
	// the client's dial settings and IP filter, as Synapse's well-known agent
	// has the federation IP blocklist.
	if srt, ok := client.HTTP.Transport.(*federation.ServerResolvingTransport); ok {
		wellKnown := *client.ExtHTTP
		wellKnown.Transport = withUserAgent(client.ExtHTTP.Transport, ua.External)
		srt.ResolveOpts = &federation.ResolveServerNameOpts{HTTPClient: &wellKnown}
	}
	client.HTTP.Transport = withUserAgent(client.HTTP.Transport, ua.Federation)
	client.ExtHTTP.Transport = withUserAgent(client.ExtHTTP.Transport, ua.External)
	// The transports override it anyway; this keeps the client's own idea of
	// it from being mautrix's.
	client.UserAgent = synapseUserAgentFallback
}

// discoverSynapseUserAgent asks serverName which Synapse it runs and returns
// the User-Agent that Synapse sends.
func discoverSynapseUserAgent(ctx context.Context, client *federation.Client, serverName string) (string, error) {
	resp, err := client.Version(ctx, serverName)
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Server.Name == "" || resp.Server.Version == "" {
		return "", fmt.Errorf("%s did not report a server name and version", serverName)
	}
	// Synapse's is "Synapse/" + SYNAPSE_VERSION. Another implementation's
	// name is used the same way rather than claiming to be Synapse.
	return resp.Server.Name + "/" + resp.Server.Version, nil
}

// RefreshUserAgent discovers the homeserver's version once, bounded by
// timeout, then keeps it current in the background until ctx ends.
func RefreshUserAgent(ctx context.Context, client *federation.Client, ua *UserAgent,
	serverName string, timeout time.Duration, log zerolog.Logger) {
	if ua.fixed {
		log.Info().Str("user_agent", ua.Federation()).Msg("Using configured outbound User-Agent")
		return
	}
	attempt := func() bool {
		actx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		base, err := discoverSynapseUserAgent(actx, client, serverName)
		if err != nil {
			log.Warn().Err(err).Str("user_agent", ua.Federation()).
				Msg("Could not learn the homeserver's version; outbound User-Agent unchanged")
			return false
		}
		if ua.set(base) {
			log.Info().Str("user_agent", base).Msg("Outbound User-Agent set from the homeserver's version")
		}
		return true
	}

	ok := attempt()
	go func() {
		for {
			// Retry soon until the version is known, then just watch for
			// upgrades.
			wait := time.Hour
			if !ok {
				wait = time.Minute
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			ok = attempt() || ok
		}
	}()
}
