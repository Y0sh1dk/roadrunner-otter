package otter

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routeTestTTL is the cache TTL applied to all test fixtures in this file.
// Long enough that no test races past it.
const routeTestTTL = 5 * time.Minute

func mustBuild(t *testing.T, cfg *config) *routeTable {
	t.Helper()
	cfg.initDefaults()
	require.NoError(t, cfg.validate())
	rt, err := buildRouteTable(cfg)
	require.NoError(t, err)

	return rt
}

// safeMethods returns a fresh slice of safe methods. It's a function (not a
// var) because InitDefaults mutates the slice in place (uppercasing entries)
// and parallel subtests would race on a shared global.
func safeMethods() []string {
	return []string{http.MethodGet, http.MethodHead}
}

func TestRouteTable_EmptyPathsBypassesEverything(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{})
	_, engage := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/anything", nil))
	assert.False(t, engage, "no path entries = middleware is a no-op")
}

func TestRouteTable_NoMatchBypasses(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{{Pattern: "^/api/", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}}},
	})
	_, engage := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/other", nil))
	assert.False(t, engage)
}

func TestRouteTable_MatchUsesEntry(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{{Pattern: "^/api/", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}}},
	})
	route, engage := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1", nil))
	require.True(t, engage)
	assert.NotNil(t, route)
	assert.NotNil(t, route.regex)
}

func TestRouteTable_DisabledEntryBypasses(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{
			{Pattern: "^/admin/", Disabled: true},
			{Pattern: "^/api/", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}},
		},
	})

	_, engage := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/users", nil))
	assert.False(t, engage, "disabled entry must cause bypass")

	_, engage = rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1", nil))
	assert.True(t, engage, "other paths still match later entries")
}

func TestRouteTable_FirstMatchWins(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{
			{Pattern: "^/api/health$", Disabled: true},
			{Pattern: "^/api/.*", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}}, // would also match /api/health
		},
	})
	_, engage := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", nil))
	assert.False(t, engage, "first matching entry wins regardless of ordering")
}

// TestRouteTable_PerEntryDefaultsApplied verifies that an entry with the
// minimum required fields (pattern + methods + cache.ttl) still gets the
// max_body_bytes default (the only field that's defaulted).
func TestRouteTable_PerEntryDefaultsApplied(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{{Pattern: "^/.*", Methods: []string{"GET"}, Cache: cacheConfig{TTL: routeTestTTL}}},
	})
	r, ok := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	require.True(t, ok)
	assert.Equal(t, []string{"GET"}, r.methods, "methods carry through verbatim — no default")
	assert.Equal(t, defaultMaxBodyBytes, r.maxBodyBytes)
	require.NotNil(t, r.cache, "cache should be initialised for active routes")

	// RFC 7234 default: 200 IS cacheable, 502 is NOT.
	assert.True(t, r.isCacheable(&snapshot{status: http.StatusOK}))
	assert.False(t, r.isCacheable(&snapshot{status: http.StatusBadGateway}))
}

// TestRouteTable_PerEntryFieldsCarryThrough verifies user-set fields land on
// the compiled route as-is.
func TestRouteTable_PerEntryFieldsCarryThrough(t *testing.T) {
	t.Parallel()

	rt := mustBuild(t, &config{
		Paths: []pathConfig{
			{
				Pattern: "^/big/",
				Methods: []string{"GET"},
				Cache: cacheConfig{
					TTL:          routeTestTTL,
					MaxBodyBytes: 1 << 20, // 1 MiB
					KeyConfig: keyConfig{
						IncludeHeaders: []string{"Authorization"},
					},
				},
			},
		},
	})

	r, ok := rt.match(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/big/file", nil))
	require.True(t, ok)
	assert.Equal(t, []string{"GET"}, r.methods)
	assert.Equal(t, int64(1<<20), r.maxBodyBytes)
	require.NotNil(t, r.kb)
	assert.Equal(t, []string{"Authorization"}, r.kb.includeHeaders)
}

// TestRouteTable_BadPatternErrors covers the defensive compile in
// buildRouteTable. Validate normally catches bad patterns first (see
// TestConfig_Validate_PathRequirements), so we skip Validate here to
// exercise the buildRouteTable error path directly.
func TestRouteTable_BadPatternErrors(t *testing.T) {
	t.Parallel()

	cfg := &config{Paths: []pathConfig{{Pattern: "[", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}}}}
	cfg.initDefaults()
	// Intentionally skip cfg.validate() — see comment above.

	_, err := buildRouteTable(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paths[0]")
}

func TestConfig_Validate_PathRequirements(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config
		want string
	}{
		{
			name: "empty pattern",
			cfg:  config{Paths: []pathConfig{{Pattern: "", Methods: safeMethods()}}},
			want: "pattern is required",
		},
		{
			name: "missing methods defaults to GET+HEAD",
			cfg:  config{Paths: []pathConfig{{Pattern: "^/x", Cache: cacheConfig{TTL: routeTestTTL}}}},
			want: "", // empty want = expect success after InitDefaults supplies methods
		},
		{
			name: "disabled entry needs no methods",
			cfg:  config{Paths: []pathConfig{{Pattern: "^/x", Disabled: true}}},
			want: "", // empty want = expect success
		},
		{
			name: "empty method in list",
			cfg:  config{Paths: []pathConfig{{Pattern: "^/x", Methods: []string{""}}}},
			want: "empty method",
		},
		{
			name: "negative cache.max_body_bytes",
			cfg:  config{Paths: []pathConfig{{Pattern: "^/x", Methods: safeMethods(), Cache: cacheConfig{MaxBodyBytes: -1}}}},
			want: "cache.max_body_bytes",
		},
		{
			name: "invalid regex pattern",
			cfg:  config{Paths: []pathConfig{{Pattern: "[", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}}}},
			want: "invalid pattern",
		},
		{
			name: "invalid regex pattern on disabled entry",
			cfg:  config{Paths: []pathConfig{{Pattern: "[", Disabled: true}}},
			want: "invalid pattern", // we check even disabled entries
		},
		{
			name: "missing cache.ttl on active entry",
			cfg:  config{Paths: []pathConfig{{Pattern: "^/x", Methods: safeMethods()}}},
			want: "cache.ttl is required",
		},
		{
			name: "negative cache.max_entries",
			cfg: config{Paths: []pathConfig{{
				Pattern: "^/x", Methods: safeMethods(),
				Cache: cacheConfig{TTL: routeTestTTL, MaxEntries: -1},
			}}},
			want: "cache.max_entries",
		},
		{
			name: "empty cache.statuses (explicitly set to empty)",
			cfg: config{Paths: []pathConfig{{
				Pattern: "^/x", Methods: safeMethods(),
				Cache: cacheConfig{TTL: routeTestTTL, Statuses: []int{}},
			}}},
			want: "cache.statuses",
		},
		{
			name: "invalid HTTP status in cache.statuses",
			cfg: config{Paths: []pathConfig{{
				Pattern: "^/x", Methods: safeMethods(),
				Cache: cacheConfig{TTL: routeTestTTL, Statuses: []int{200, 9999}},
			}}},
			want: "invalid HTTP status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.cfg.initDefaults()

			err := tc.cfg.validate()
			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
