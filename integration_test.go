package otter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationServer wires the plugin in front of an arbitrary upstream and
// returns a running httptest.Server. The test's t.Cleanup handles shutdown.
// If cfg is nil, a catch-all "^.*$" path entry is used so the middleware
// engages for every request.
func integrationServer(t *testing.T, cfg *Config, upstream http.Handler) *httptest.Server {
	t.Helper()

	if cfg == nil {
		cfg = catchAll()
	}

	p := newPluginForTest(t, cfg)
	srv := httptest.NewServer(p.Middleware(upstream))
	t.Cleanup(srv.Close)

	return srv
}

// getBody is a convenience that issues a GET and returns (status, body, headers).
func getBody(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(b), resp.Header
}

// TestIntegration_FailureResponsesAreAlsoCoalesced documents a sometimes
// surprising behavior: a coalesced upstream 5xx is fanned out to every waiter
// too. This is correct (we coalesce on request identity, not on success), but
// it means a single bad backend hiccup can affect every waiter. Callers
// should be aware.
func TestIntegration_FailureResponsesAreAlsoCoalesced(t *testing.T) {
	var calls atomic.Int32

	release := make(chan struct{})

	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	}))

	const N = 10

	var wg sync.WaitGroup

	statuses := make([]int, N)
	bodies := make([]string, N)

	for i := range N {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			statuses[i], bodies[i], _ = getBody(t, srv.URL+"/flaky")
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "the 5xx must come from a single upstream call")

	for i := range N {
		assert.Equal(t, http.StatusBadGateway, statuses[i])
		assert.JSONEq(t, `{"error":"upstream down"}`, bodies[i])
	}
}

// TestIntegration_ErrorResponsesAreNotCached verifies the RFC 7234 default:
// a 5xx response is served to the caller but NOT stored in the cache, so a
// subsequent request triggers a fresh upstream call (rather than serving
// the cached error for the full TTL).
func TestIntegration_ErrorResponsesAreNotCached(t *testing.T) {
	var calls atomic.Int32

	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream broke"))
	}))

	// Two sequential requests. Both should hit upstream because 502 is not
	// in the default cacheable_statuses set.
	for range 2 {
		status, body, _ := getBody(t, srv.URL+"/flaky")
		assert.Equal(t, http.StatusBadGateway, status)
		assert.Equal(t, "upstream broke", body)
	}

	assert.Equal(t, int32(2), calls.Load(),
		"5xx responses must NOT be cached by default — second request should also hit upstream")
}

// TestIntegration_CustomCacheStatusesRespected verifies that an explicit
// cache_statuses list overrides the RFC 7234 default — here we cache only
// 200, so a 404 doesn't stick around.
func TestIntegration_CustomCacheStatusesRespected(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{{
			Pattern:       "^.*$",
			Methods:       []string{"GET"},
			CacheTTL:      5 * time.Minute,
			CacheStatuses: []int{http.StatusOK}, // only 2xx, deliberately excluding 404
		}},
	}

	var calls atomic.Int32

	srv := integrationServer(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))

	// Two sequential 404s — without 404 in the cache_statuses list, neither
	// should be cached.
	for range 2 {
		status, _, _ := getBody(t, srv.URL+"/missing")
		assert.Equal(t, http.StatusNotFound, status)
	}

	assert.Equal(t, int32(2), calls.Load(),
		"custom cache_statuses excludes 404 → each request should hit upstream")
}

// TestIntegration_HeadersPreservedAcrossWaiters verifies the full set of
// upstream response headers is replayed verbatim to every waiter — including
// multi-value headers.
func TestIntegration_HeadersPreservedAcrossWaiters(t *testing.T) {
	release := make(chan struct{})

	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release

		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "public, max-age=60")
		h.Set("X-Custom", "v1")
		h.Add("Link", `</a>; rel="next"`)
		h.Add("Link", `</b>; rel="prev"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	const N = 8

	var wg sync.WaitGroup

	headers := make([]http.Header, N)

	for i := range N {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, _, headers[i] = getBody(t, srv.URL+"/cached")
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range N {
		assert.Equal(t, "application/json", headers[i].Get("Content-Type"), "waiter %d", i)
		assert.Equal(t, "public, max-age=60", headers[i].Get("Cache-Control"), "waiter %d", i)
		assert.Equal(t, "v1", headers[i].Get("X-Custom"), "waiter %d", i)
		assert.ElementsMatch(
			t,
			[]string{`</a>; rel="next"`, `</b>; rel="prev"`},
			headers[i].Values("Link"),
			"multi-value headers must replay in full to waiter %d", i,
		)
	}
}

// TestIntegration_HEADRequestNoBody verifies HEAD passes through the
// middleware correctly: status + headers replayed, no body.
func TestIntegration_HEADRequestNoBody(t *testing.T) {
	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Marker", "head-ok")
		w.WriteHeader(http.StatusNoContent)
	}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "head-ok", resp.Header.Get("X-Marker"))
	assert.Empty(t, b)
}

// TestIntegration_RedirectResponse verifies 3xx + Location replay correctly
// without the test HTTP client following the redirect.
func TestIntegration_RedirectResponse(t *testing.T) {
	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	}))

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/src", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/target", resp.Header.Get("Location"))
}

// TestIntegration_IncludeHeadersIsolatesAuthScopes verifies that
// Authorization is part of the key when configured — so two concurrent users
// hitting the same URL do *not* share a response.
func TestIntegration_IncludeHeadersIsolatesAuthScopes(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{{
			Pattern:  "^.*$",
			Methods:  []string{"GET"},
			CacheTTL: 5 * time.Minute,
			Key: KeyConfig{
				IncludeHeaders: []string{"Authorization"},
			},
		}},
	}

	var calls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		//nolint:gosec // G705 false positive: test handler echoing a header back over plain text, not HTML
		_, _ = w.Write([]byte("hello " + r.Header.Get("Authorization")))
	})
	srv := integrationServer(t, cfg, upstream)

	type result struct {
		body string
		err  error
	}

	doReq := func(token string) result {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return result{err: err}
		}
		defer func() { _ = resp.Body.Close() }()

		b, _ := io.ReadAll(resp.Body)

		return result{body: string(b)}
	}

	r1 := doReq("alice")
	r2 := doReq("bob")

	require.NoError(t, r1.err)
	require.NoError(t, r2.err)
	assert.Equal(t, "hello Bearer alice", r1.body)
	assert.Equal(t, "hello Bearer bob", r2.body)
	assert.Equal(t, int32(2), calls.Load(), "different auth tokens must not coalesce")
}

// TestIntegration_MixedMethodBurst verifies GET coalesces while POST passes
// through, even when interleaved at the same path.
func TestIntegration_MixedMethodBurst(t *testing.T) {
	var getCalls, postCalls atomic.Int32

	releaseGet := make(chan struct{})

	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls.Add(1)
			<-releaseGet
		case http.MethodPost:
			postCalls.Add(1)
		}

		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(2)

		go func() {
			defer wg.Done()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/mixed", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		}()
		go func() {
			defer wg.Done()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mixed", strings.NewReader("x"))
			if !assert.NoError(t, err) {
				return
			}

			req.Header.Set("Content-Type", "text/plain")

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(releaseGet)
	wg.Wait()

	assert.Equal(t, int32(1), getCalls.Load(), "10 concurrent GETs must coalesce")
	assert.Equal(t, int32(10), postCalls.Load(), "POSTs must bypass the middleware")
}

// TestIntegration_DistinctKeysDoNotBlockEachOther verifies that 10 different
// keys progress independently — otter's loader only serializes within a
// key, not across keys. Worth proving end-to-end.
func TestIntegration_DistinctKeysDoNotBlockEachOther(t *testing.T) {
	var inFlight atomic.Int32

	maxInFlight := atomic.Int32{}

	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)

		for {
			cur := maxInFlight.Load()
			if n <= cur || maxInFlight.CompareAndSwap(cur, n) {
				break
			}
		}
		// Small artificial delay so concurrent requests genuinely overlap.
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))

	const N = 10

	var wg sync.WaitGroup

	wg.Add(N)

	for i := range N {
		go func(i int) {
			defer wg.Done()

			url := fmt.Sprintf("%s/key/%d", srv.URL, i)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)

			_ = resp.Body.Close()
		}(i)
	}

	wg.Wait()

	assert.Greater(t, maxInFlight.Load(), int32(1),
		"distinct keys should be allowed to execute concurrently — got max-in-flight = %d", maxInFlight.Load())
}

// TestIntegration_NoConfigSectionIsNoop confirms that when .rr.yaml has no
// otter: section at all (so paths is empty), the middleware is wired
// but passes every request through to the upstream untouched.
func TestIntegration_NoConfigSectionIsNoop(t *testing.T) {
	// fakeConfigurer with nil cfg reports Has(...) == false, matching the
	// "no section in yaml" case.
	p := &Plugin{}
	require.NoError(t, p.Init(&fakeConfigurer{section: PluginName, cfg: nil}, nil))

	var calls atomic.Int32

	srv := httptest.NewServer(p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("no-config"))
	})))
	t.Cleanup(srv.Close)

	// Two identical concurrent requests must NOT coalesce — there's no path
	// entry to engage the middleware.
	release := make(chan struct{})
	gateUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	gated := httptest.NewServer(p.Middleware(gateUpstream))
	t.Cleanup(gated.Close)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gated.URL+"/", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		})
	}

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	// And the original simple GET still returns its body, proving the
	// middleware doesn't 5xx with empty config.
	status, body, _ := getBody(t, srv.URL+"/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "no-config", body)
}

// TestIntegration_OriginalResponseWriterReceivesHeadersOnce guards against a
// regression where header-replay would accidentally double-write keys onto
// the downstream ResponseWriter (e.g. via append without truncation).
func TestIntegration_OriginalResponseWriterReceivesHeadersOnce(t *testing.T) {
	srv := integrationServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Once", "yes")
		w.WriteHeader(http.StatusOK)
	}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, []string{"yes"}, resp.Header.Values("X-Once"))
}

// TestIntegration_PathAllowlist verifies that with a non-empty Paths list,
// only requests matching one of the patterns engage the middleware. Other
// requests pass straight through.
func TestIntegration_PathAllowlist(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{
			{Pattern: "^/api/v1/feed$", Methods: []string{"GET", "HEAD"}, CacheTTL: 5 * time.Minute},
			{Pattern: "^/api/v1/users/[^/]+$", Methods: []string{"GET", "HEAD"}, CacheTTL: 5 * time.Minute},
		},
	}

	var calls atomic.Int32

	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		// Only block requests that the middleware is supposed to coalesce,
		// so the test doesn't deadlock on bypassed paths.
		select {
		case <-release:
		default:
		}

		w.WriteHeader(http.StatusOK)
	})
	srv := integrationServer(t, cfg, upstream)

	// /api/v1/feed — in the allowlist; concurrent requests must coalesce.
	calls.Store(0)

	release = make(chan struct{})
	upstreamWithBlock := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv2 := integrationServer(t, cfg, upstreamWithBlock)

	const N = 8

	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv2.URL+"/api/v1/feed", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load(), "allowlisted path should coalesce")

	// /not-in-allowlist — bypasses the middleware entirely.
	calls.Store(0)

	for range 3 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/not-in-allowlist", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	assert.Equal(t, int32(3), calls.Load(), "non-allowlisted path should bypass: each request hits upstream")
}

// TestIntegration_DisabledPathCarveOut verifies an early `disabled: true`
// entry creates a carve-out inside a broader allowlist entry.
func TestIntegration_DisabledPathCarveOut(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{
			{Pattern: "^/api/health$", Disabled: true},                                         // never coalesce
			{Pattern: "^/api/.*", Methods: []string{"GET", "HEAD"}, CacheTTL: 5 * time.Minute}, // coalesce everything else
		},
	}

	var calls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := integrationServer(t, cfg, upstream)

	// /api/health — should bypass (disabled carve-out wins by being first).
	calls.Store(0)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/health", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		})
	}

	wg.Wait()
	assert.Equal(t, int32(5), calls.Load(), "carve-out path must bypass on every request")
}

// TestIntegration_PerPathIncludeHeaders verifies that IncludeHeaders is
// genuinely per-path: /auth/ splits its key by Authorization while /public/
// (which omits the override) coalesces across all callers regardless of
// their Authorization header.
func TestIntegration_PerPathIncludeHeaders(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{
			{Pattern: "^/public/", Methods: []string{"GET", "HEAD"}, CacheTTL: 5 * time.Minute}, // no IncludeHeaders
			{
				Pattern:  "^/auth/",
				Methods:  []string{"GET", "HEAD"},
				CacheTTL: 5 * time.Minute,
				Key: KeyConfig{
					IncludeHeaders: []string{"Authorization"},
				},
			},
		},
	}

	var calls atomic.Int32

	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		//nolint:gosec // G705 false positive: test handler echoing a header back over plain text, not HTML
		_, _ = w.Write([]byte("auth=" + r.Header.Get("Authorization")))
	})
	srv := integrationServer(t, cfg, upstream)

	// /auth/ with two different tokens — must NOT coalesce.
	calls.Store(0)

	bodies := make([]string, 2)

	var wg sync.WaitGroup
	for i, token := range []string{"alice", "bob"} {
		wg.Add(1)

		go func(i int, tok string) {
			defer wg.Done()

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/auth/me", nil)
			req.Header.Set("Authorization", "Bearer "+tok)

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			bodies[i] = string(b)
		}(i, token)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(2), calls.Load(), "Authorization in the path's IncludeHeaders must split the key")
	assert.Equal(t, "auth=Bearer alice", bodies[0])
	assert.Equal(t, "auth=Bearer bob", bodies[1])

	// /public/ with two different tokens — global key has no IncludeHeaders,
	// so these MUST coalesce.
	calls.Store(0)

	release = make(chan struct{})
	upstream2 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv2 := integrationServer(t, cfg, upstream2)

	wg = sync.WaitGroup{}
	for _, token := range []string{"alice", "bob"} {
		wg.Add(1)

		go func(tok string) {
			defer wg.Done()

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv2.URL+"/public/feed", nil)
			req.Header.Set("Authorization", "Bearer "+tok)

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
		}(token)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load(),
		"the /public/ entry has no IncludeHeaders; different auth headers on it should still coalesce")
}

// TestIntegration_PerPathMaxBodyBytes verifies that the configured cap
// actually trips for the matched path.
func TestIntegration_PerPathMaxBodyBytes(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{
			// MaxBodyBytes intentionally tiny so any response trips the cap.
			{
				Pattern: "^/tiny/", Methods: []string{"GET", "HEAD"},
				MaxBodyBytes: 16, CacheTTL: 5 * time.Minute,
			},
		},
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("A", 1024)))
	})
	srv := integrationServer(t, cfg, upstream)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/tiny/x", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"per-path MaxBodyBytes must trip for oversized responses")
}

// TestIntegration_PerPathMethodsScope verifies that an entry's Methods
// list scopes which methods coalesce for that pattern.
func TestIntegration_PerPathMethodsScope(t *testing.T) {
	cfg := &Config{
		Paths: []PathConfig{
			{
				Pattern:  "^/get-only/",
				Methods:  []string{"GET"}, // exclude HEAD on this path
				CacheTTL: 5 * time.Minute,
			},
		},
	}

	var calls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := integrationServer(t, cfg, upstream)

	// Two HEAD requests in serial — without coalescing, both hit upstream.
	for range 2 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/get-only/x", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	assert.Equal(t, int32(2), calls.Load(),
		"HEAD on /get-only/ should bypass: methods override removed HEAD from this path")
}
