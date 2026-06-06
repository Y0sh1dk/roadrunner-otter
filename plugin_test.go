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

// fakeConfigurer is a minimal Configurer for plugin tests.
type fakeConfigurer struct {
	section string
	cfg     *Config
}

func (f *fakeConfigurer) Has(name string) bool { return f.cfg != nil && name == f.section }

func (f *fakeConfigurer) UnmarshalKey(name string, out any) error {
	if name != f.section || f.cfg == nil {
		return fmt.Errorf("no config for %q", name)
	}

	dst, ok := out.(*Config)
	if !ok {
		return fmt.Errorf("unexpected target type %T", out)
	}

	*dst = *f.cfg

	return nil
}

func newPluginForTest(t *testing.T, cfg *Config) *Plugin {
	t.Helper()

	p := &Plugin{}

	var c Configurer
	if cfg != nil {
		c = &fakeConfigurer{section: PluginName, cfg: cfg}
	}

	require.NoError(t, p.Init(c, nil))

	return p
}

// testTTL is the cache TTL used by test fixtures. Long enough that no test
// in this package races past it; short enough that a "wait for expiry" test
// stays fast if we add one.
const testTTL = 5 * time.Minute

// catchAll returns a Config with a single "^.*$" path entry covering both
// safe methods. Used by tests that just want "the middleware engages for
// every request, default cap & header.".
func catchAll() *Config {
	return &Config{Paths: []PathConfig{{
		Pattern: "^.*$",
		Methods: []string{"GET", "HEAD"},
		Cache:   CacheConfig{TTL: testTTL},
	}}}
}

func TestPlugin_Init_DefaultsAppliedPerEntry(t *testing.T) {
	t.Parallel()

	// Verify the defaulting pass: an entry that only sets the required
	// pattern + cache.ttl picks up methods, max_body_bytes, max_entries
	// from InitDefaults.
	p := newPluginForTest(t, &Config{
		Paths: []PathConfig{{
			Pattern: "^/.*",
			Methods: []string{"GET"},
			Cache:   CacheConfig{TTL: testTTL},
		}},
	})
	require.Len(t, p.config.Paths, 1)
	assert.Equal(t, []string{"GET"}, p.config.Paths[0].Methods, "explicit methods carry through verbatim")
	assert.Equal(t, defaultMaxBodyBytes, p.config.Paths[0].Cache.MaxBodyBytes)
	assert.Equal(t, defaultCacheMaxEntries, p.config.Paths[0].Cache.MaxEntries)
}

func TestPlugin_Init_MethodsDefaultedWhenOmitted(t *testing.T) {
	t.Parallel()

	// An active entry with no Methods picks up the safe-cacheable default
	// (GET + HEAD) rather than failing validation.
	p := newPluginForTest(t, &Config{
		Paths: []PathConfig{{
			Pattern: "^/.*",
			Cache:   CacheConfig{TTL: testTTL},
		}},
	})
	assert.Equal(t, []string{http.MethodGet, http.MethodHead}, p.config.Paths[0].Methods)
}

func TestPlugin_Init_DisabledEntryNeedsNoMethods(t *testing.T) {
	t.Parallel()

	// A disabled entry doesn't use Methods; validation must allow it.
	p := &Plugin{}
	cfg := &fakeConfigurer{section: PluginName, cfg: &Config{
		Paths: []PathConfig{{Pattern: "^/admin/", Disabled: true}},
	}}
	require.NoError(t, p.Init(cfg, nil))
}

func TestPlugin_Init_AppliesPerPathConfig(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, &Config{
		Paths: []PathConfig{{
			Pattern: "^/.*",
			Methods: []string{"get", "post"},
			Cache:   CacheConfig{TTL: testTTL, MaxBodyBytes: 1024},
		}},
	})
	got := p.config.Paths[0]
	assert.Equal(t, []string{"GET", "POST"}, got.Methods, "methods are uppercased")
	assert.Equal(t, int64(1024), got.Cache.MaxBodyBytes)

	rt, _ := p.routes.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	require.NotNil(t, rt)
	assert.Equal(t, int64(1024), rt.maxBodyBytes, "override carries through to compiled route")
}

func TestPlugin_Init_NoPathsIsNoop(t *testing.T) {
	t.Parallel()

	// Empty Paths = middleware is wired but does nothing. This is allowed
	// but unusual; the middleware should pass every request through.
	p := newPluginForTest(t, &Config{})

	var calls int

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++

		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	p.Middleware(upstream).ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/anything", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, calls, "empty paths must still pass requests through to upstream")
}

func TestPlugin_Name(t *testing.T) {
	t.Parallel()

	p := &Plugin{}
	assert.Equal(t, PluginName, p.Name())
}

// TestPlugin_CoalescesConcurrentRequests verifies the core promise: N
// concurrent GETs for the same key result in exactly one upstream call.
func TestPlugin_CoalescesConcurrentRequests(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, catchAll())

	var upstreamCalls atomic.Int32

	release := make(chan struct{})

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		<-release // hold the upstream open so concurrent requests pile up
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	handler := p.Middleware(upstream)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	const N = 25

	var wg sync.WaitGroup

	results := make([]*http.Response, N)
	bodies := make([]string, N)

	for i := range N {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/coalesce-me", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.NoError(t, err) {
				return
			}

			_ = resp.Body.Close()
			results[i] = resp
			bodies[i] = string(b)
		}(i)
	}

	// Give all goroutines a beat to enter the cache.Get loader before we
	// release upstream. This is best-effort; the test would still pass if a
	// few races happened, but we'd see >1 upstream call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), upstreamCalls.Load(), "all requests must coalesce into one upstream call")

	for i := range N {
		assert.Equal(t, http.StatusOK, results[i].StatusCode)
		assert.Equal(t, "hello", bodies[i])
	}
}

func TestPlugin_BypassesNonCoalescedMethods(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, catchAll())

	var upstreamCalls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	handler := p.Middleware(upstream)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	for range 3 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/x", strings.NewReader("data"))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	assert.Equal(t, int32(3), upstreamCalls.Load(), "POSTs should bypass the middleware by default")
}

func TestPlugin_DifferentKeysDoNotCoalesce(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, catchAll())

	var upstreamCalls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		//nolint:gosec // G705 false positive: test handler echoing a path back over plain text, not HTML
		_, _ = w.Write([]byte(r.URL.Path))
	})

	handler := p.Middleware(upstream)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	for _, p := range []string{"/a", "/b", "/c"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+p, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	assert.Equal(t, int32(3), upstreamCalls.Load())
}

// TestPlugin_SerialRequestsHitCache verifies the new caching behavior: two
// sequential requests for the same key result in just one upstream call,
// because the second is served from the otter cache. This is the difference
// from the old pure-in-flight-dedup design (no caching between calls).
func TestPlugin_SerialRequestsHitCache(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, catchAll())

	var upstreamCalls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cached"))
	})

	handler := p.Middleware(upstream)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	for range 5 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/same", nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, "cached", string(body))
	}

	assert.Equal(t, int32(1), upstreamCalls.Load(),
		"five identical requests within TTL should hit upstream once and serve four from cache")
}

func TestPlugin_OversizedResponseReturns502(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, &Config{
		Paths: []PathConfig{{
			Pattern: "^.*$",
			Methods: []string{"GET", "HEAD"},
			Cache:   CacheConfig{TTL: testTTL, MaxBodyBytes: 16},
		}},
	})

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("A", 1024)))
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/big", nil)
	p.Middleware(upstream).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	c := &Config{Paths: []PathConfig{{
		Pattern: "^/x",
		Methods: []string{"GET"},
		Cache:   CacheConfig{TTL: testTTL, MaxBodyBytes: -1},
	}}}
	c.InitDefaults()
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache.max_body_bytes")

	// InitDefaults uppercases an empty string, which is still empty —
	// Validate must catch it.
	c = &Config{Paths: []PathConfig{{Pattern: "^/x", Methods: []string{""}, Cache: CacheConfig{TTL: testTTL}}}}
	c.InitDefaults()
	err = c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty method")
}
