package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// authclient.go validates client access tokens by asking Synapse.
//
// The worker deliberately does not read the access_tokens table. Appservice
// tokens live in registration files rather than the database, and delegated
// auth (MAS) keeps tokens outside Synapse entirely, so a direct table lookup
// would reject valid callers. Asking Synapse is authoritative in every
// deployment, and caching makes it cheap.
//
// The cache is in-memory only and is never persisted: it holds credentials,
// refills in milliseconds, and writing it to disk would be a liability with no
// upside.

type tokenVerdict struct {
	valid   bool
	userID  string
	isGuest bool
	expires time.Time
}

type cacheEntry struct {
	key     string
	verdict tokenVerdict
}

// TokenAuthenticator validates access tokens against Synapse's whoami endpoint,
// caching both successes and rejections.
type TokenAuthenticator struct {
	client      *http.Client
	whoamiURL   string
	positiveTTL time.Duration
	negativeTTL time.Duration
	maxEntries  int

	group singleflight.Group

	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List // front = most recently used
}

func NewTokenAuthenticator(cfg AuthConfig) (*TokenAuthenticator, error) {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	base := cfg.WhoamiURL
	if cfg.WhoamiSocket != "" {
		socket := cfg.WhoamiSocket
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		if base == "" {
			// The host is ignored once the dialer is pinned to a socket, but
			// net/http still needs a syntactically valid URL.
			base = "http://synapse"
		}
	}
	if base == "" {
		return nil, fmt.Errorf("auth: whoami_url or whoami_socket must be set")
	}
	return &TokenAuthenticator{
		client:      &http.Client{Transport: transport, Timeout: 10 * time.Second},
		whoamiURL:   strings.TrimRight(base, "/") + "/_matrix/client/v3/account/whoami",
		positiveTTL: cfg.PositiveTTL,
		negativeTTL: cfg.NegativeTTL,
		maxEntries:  cfg.MaxEntries,
		items:       make(map[string]*list.Element),
		order:       list.New(),
	}, nil
}

// ExtractToken pulls the access token from the request, accepting both the
// Authorization header and the legacy query parameter.
func ExtractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if token, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
		return ""
	}
	return r.URL.Query().Get("access_token")
}

// hashToken keys the cache without holding raw credentials in memory longer
// than the request that carried them.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type whoamiResponse struct {
	UserID  string `json:"user_id"`
	IsGuest bool   `json:"is_guest"`
}

// Authenticate validates a token, returning the verdict. A non-nil error means
// the answer is unknown (Synapse unreachable), which callers should surface as
// 503 rather than 401.
func (a *TokenAuthenticator) Authenticate(ctx context.Context, token string) (tokenVerdict, error) {
	if token == "" {
		return tokenVerdict{valid: false}, nil
	}
	key := hashToken(token)
	if v, ok := a.lookup(key); ok {
		return v, nil
	}

	// singleflight collapses the stampede when a client reconnects many
	// sessions at once with the same token.
	res, err, _ := a.group.Do(key, func() (any, error) {
		if v, ok := a.lookup(key); ok {
			return v, nil
		}
		v, err := a.callWhoami(ctx, token)
		if err != nil {
			return tokenVerdict{}, err
		}
		a.store(key, v)
		return v, nil
	})
	if err != nil {
		return tokenVerdict{}, err
	}
	return res.(tokenVerdict), nil
}

func (a *TokenAuthenticator) callWhoami(ctx context.Context, token string) (tokenVerdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.whoamiURL, nil)
	if err != nil {
		return tokenVerdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.client.Do(req)
	if err != nil {
		return tokenVerdict{}, fmt.Errorf("whoami request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		var body whoamiResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return tokenVerdict{}, fmt.Errorf("decoding whoami response: %w", err)
		}
		return tokenVerdict{
			valid:   true,
			userID:  body.UserID,
			isGuest: body.IsGuest,
			expires: time.Now().Add(a.positiveTTL),
		}, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return tokenVerdict{valid: false, expires: time.Now().Add(a.negativeTTL)}, nil
	default:
		// Anything else (5xx, ratelimit) is an unknown answer, not a rejection.
		return tokenVerdict{}, fmt.Errorf("whoami returned unexpected status %d", resp.StatusCode)
	}
}

func (a *TokenAuthenticator) lookup(key string) (tokenVerdict, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	elem, ok := a.items[key]
	if !ok {
		return tokenVerdict{}, false
	}
	entry := elem.Value.(*cacheEntry)
	if time.Now().After(entry.verdict.expires) {
		a.order.Remove(elem)
		delete(a.items, key)
		return tokenVerdict{}, false
	}
	a.order.MoveToFront(elem)
	return entry.verdict, true
}

func (a *TokenAuthenticator) store(key string, v tokenVerdict) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if elem, ok := a.items[key]; ok {
		elem.Value.(*cacheEntry).verdict = v
		a.order.MoveToFront(elem)
		return
	}
	elem := a.order.PushFront(&cacheEntry{key: key, verdict: v})
	a.items[key] = elem
	for a.maxEntries > 0 && a.order.Len() > a.maxEntries {
		oldest := a.order.Back()
		if oldest == nil {
			break
		}
		a.order.Remove(oldest)
		delete(a.items, oldest.Value.(*cacheEntry).key)
	}
}

// Len reports the number of cached verdicts, for metrics.
func (a *TokenAuthenticator) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.order.Len()
}
