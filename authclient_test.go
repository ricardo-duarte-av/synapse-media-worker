package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestAuth(t *testing.T, handler http.HandlerFunc) (*TokenAuthenticator, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a, err := NewTokenAuthenticator(AuthConfig{
		WhoamiURL:   srv.URL,
		PositiveTTL: time.Minute,
		NegativeTTL: time.Minute,
		MaxEntries:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, srv
}

func TestAuthenticateCachesSuccess(t *testing.T) {
	var calls int32
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com","is_guest":false}`))
	})
	for range 5 {
		v, err := a.Authenticate(context.Background(), "tok")
		if err != nil || !v.valid || v.userID != "@a:example.com" {
			t.Fatalf("verdict=%+v err=%v", v, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("whoami called %d times, want 1", got)
	}
}

func TestAuthenticateCachesRejection(t *testing.T) {
	var calls int32
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	for range 3 {
		v, err := a.Authenticate(context.Background(), "bad")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.valid {
			t.Fatal("invalid token accepted")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("whoami called %d times, want 1", got)
	}
}

// A 5xx from Synapse is an unknown answer, not a rejection, and must not be
// cached as one: that would lock out every user for the negative TTL.
func TestServerErrorIsNotCachedAsRejection(t *testing.T) {
	var calls int32
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com"}`))
	})
	if _, err := a.Authenticate(context.Background(), "tok"); err == nil {
		t.Fatal("expected an error for a 502 whoami")
	}
	v, err := a.Authenticate(context.Background(), "tok")
	if err != nil || !v.valid {
		t.Fatalf("retry after transient failure: verdict=%+v err=%v", v, err)
	}
}

// Many concurrent requests bearing the same token must produce one whoami call.
func TestConcurrentRequestsCollapse(t *testing.T) {
	var calls int32
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com"}`))
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Authenticate(context.Background(), "tok"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("whoami called %d times, want 1", got)
	}
}

func TestExpiredEntryIsRefetched(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com"}`))
	}))
	defer srv.Close()
	a, err := NewTokenAuthenticator(AuthConfig{
		WhoamiURL: srv.URL, PositiveTTL: 10 * time.Millisecond,
		NegativeTTL: time.Minute, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := a.Authenticate(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("whoami called %d times, want 2", got)
	}
}

func TestLRUEviction(t *testing.T) {
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com"}`))
	})
	a.maxEntries = 3
	for _, tok := range []string{"a", "b", "c", "d", "e"} {
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.Len(); got != 3 {
		t.Errorf("cache holds %d entries, want 3", got)
	}
}

func TestExtractToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := ExtractToken(r); got != "abc123" {
		t.Errorf("bearer: got %q", got)
	}
	// A non-Bearer scheme is not an access token.
	r.Header.Set("Authorization", "Basic abc123")
	if got := ExtractToken(r); got != "" {
		t.Errorf("basic auth should yield no token, got %q", got)
	}
	// Legacy clients still pass the token in the query string.
	r2 := httptest.NewRequest("GET", "/x?access_token=q1", nil)
	if got := ExtractToken(r2); got != "q1" {
		t.Errorf("query token: got %q", got)
	}
	r3 := httptest.NewRequest("GET", "/x", nil)
	if got := ExtractToken(r3); got != "" {
		t.Errorf("no token: got %q", got)
	}
}

// The cache must never be keyed by the raw token.
func TestCacheKeyIsHashed(t *testing.T) {
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com"}`))
	})
	const secret = "super-secret-token"
	if _, err := a.Authenticate(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.items {
		if key == secret {
			t.Fatal("raw access token used as cache key")
		}
	}
}

// Synapse distinguishes an appservice masquerading outside its namespace from
// one naming a user it never registered. Flattening both into a generic
// message loses the only information that tells a bridge which mistake it made.
func TestRejectionIsPassedThrough(t *testing.T) {
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN",` +
			`"error":"Application service has not registered this user (@ghost:example.com)"}`))
	})
	v, err := a.AuthenticateAs(context.Background(), Credentials{
		Token: "as_token", UserID: "@ghost:example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.valid {
		t.Fatal("a refused masquerade was accepted")
	}
	if v.rejection == nil {
		t.Fatal("Synapse's rejection was discarded")
	}
	if v.rejection.ErrCode != "M_FORBIDDEN" {
		t.Errorf("errcode = %q", v.rejection.ErrCode)
	}
	if !strings.Contains(v.rejection.Error, "has not registered this user") {
		t.Errorf("message lost: %q", v.rejection.Error)
	}
	if v.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", v.status)
	}
}

// A plain invalid token has no useful upstream body; the worker must still
// answer, not depend on one being present.
func TestRejectionWithoutBody(t *testing.T) {
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	v, err := a.Authenticate(context.Background(), "bad")
	if err != nil {
		t.Fatal(err)
	}
	if v.valid {
		t.Fatal("accepted an invalid token")
	}
	if v.rejection != nil {
		t.Errorf("invented a rejection body: %+v", v.rejection)
	}
}

// The masquerade parameters must reach Synapse, or it resolves the appservice
// bot instead of the ghost and the upload is attributed to the wrong user.
func TestMasqueradeParametersAreForwarded(t *testing.T) {
	var gotUser, gotDevice string
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.URL.Query().Get("user_id")
		gotDevice = r.URL.Query().Get("device_id")
		_, _ = w.Write([]byte(`{"user_id":"` + gotUser + `"}`))
	})
	v, err := a.AuthenticateAs(context.Background(), Credentials{
		Token: "as_token", UserID: "@signal_x:example.com", DeviceID: "DEV1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUser != "@signal_x:example.com" {
		t.Errorf("user_id forwarded as %q", gotUser)
	}
	if gotDevice != "DEV1" {
		t.Errorf("device_id forwarded as %q", gotDevice)
	}
	if v.userID != "@signal_x:example.com" {
		t.Errorf("resolved to %q, want the ghost", v.userID)
	}
}

// One appservice token resolves to many users, so the cache must not serve one
// ghost's verdict for another.
func TestCacheKeyIncludesMasquerade(t *testing.T) {
	var calls int
	a, _ := newTestAuth(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"user_id":"` + r.URL.Query().Get("user_id") + `"}`))
	})
	first, _ := a.AuthenticateAs(context.Background(), Credentials{Token: "t", UserID: "@a:example.com"})
	second, _ := a.AuthenticateAs(context.Background(), Credentials{Token: "t", UserID: "@b:example.com"})
	if first.userID == second.userID {
		t.Fatalf("both resolved to %q; the cache is keyed on the token alone", first.userID)
	}
	if calls != 2 {
		t.Errorf("whoami called %d times, want one per distinct user", calls)
	}
}
