package otter

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherByPath registers c with a fresh registry and returns a
// path-label-keyed map of the named metric's sample values. Counter and
// gauge are both projected to float64.
func gatherByPath(t *testing.T, c prometheus.Collector, metricName string) map[string]float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))

	mfs, err := reg.Gather()
	require.NoError(t, err)

	out := make(map[string]float64)

	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}

		for _, m := range mf.GetMetric() {
			var label string

			for _, lp := range m.GetLabel() {
				if lp.GetName() == "path" {
					label = lp.GetValue()

					break
				}
			}

			//nolint:exhaustive // only Counter/Gauge are emitted by this collector
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				out[label] = m.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				out[label] = m.GetGauge().GetValue()
			default:
				t.Fatalf("unexpected metric type %v for %s", mf.GetType(), metricName)
			}
		}
	}

	return out
}

// TestMetricsCollector_NamePreferredOverPattern verifies that the friendly
// Name (when set) wins over the regex Pattern for the `path` label.
func TestMetricsCollector_NamePreferredOverPattern(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, &config{
		Paths: []pathConfig{
			{
				Pattern: "^/api/v1/users/[^/]+$",
				Name:    "users",
				Methods: []string{"GET"},
				Cache:   cacheConfig{TTL: 5 * time.Minute},
			},
			{
				Pattern: "^/api/v1/feed$",
				Methods: []string{"GET"},
				Cache:   cacheConfig{TTL: 5 * time.Minute},
			},
		},
	})

	sizes := gatherByPath(t, p.MetricsCollector()[0], "otter_cache_size")
	assert.Contains(t, sizes, "users", "named path uses the friendly label")
	assert.Contains(t, sizes, "^/api/v1/feed$", "unnamed path falls back to the regex")
}

// TestMetricsCollector_DisabledPathsHaveNoMetrics verifies disabled entries
// emit no samples (they have no cache to read from).
func TestMetricsCollector_DisabledPathsHaveNoMetrics(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, &config{
		Paths: []pathConfig{
			{Pattern: "^/api/health$", Name: "health", Disabled: true},
			{
				Pattern: "^/api/v1/feed$",
				Name:    "feed",
				Methods: []string{"GET"},
				Cache:   cacheConfig{TTL: 5 * time.Minute},
			},
		},
	})

	hits := gatherByPath(t, p.MetricsCollector()[0], "otter_cache_hits_total")
	assert.Contains(t, hits, "feed")
	assert.NotContains(t, hits, "health", "disabled entries are skipped by the collector")
}

// TestMetricsCollector_HitsAndMissesAdvance drives traffic through the
// middleware and verifies that the recorded stats reach the collector.
func TestMetricsCollector_HitsAndMissesAdvance(t *testing.T) {
	t.Parallel()

	p := newPluginForTest(t, catchAll())

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(p.Middleware(upstream))
	t.Cleanup(srv.Close)

	// 1 miss (warming the cache) + 2 hits on the same key.
	for range 3 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/same", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	c := p.MetricsCollector()[0]
	hits := gatherByPath(t, c, "otter_cache_hits_total")
	misses := gatherByPath(t, c, "otter_cache_misses_total")
	loads := gatherByPath(t, c, "otter_cache_load_successes_total")

	require.Len(t, hits, 1, "one series for the catch-all path")
	assert.InDelta(t, 2.0, hits["^.*$"], 0.0001, "second and third requests hit cache")
	assert.InDelta(t, 1.0, misses["^.*$"], 0.0001, "first request was a miss")
	assert.InDelta(t, 1.0, loads["^.*$"], 0.0001, "exactly one upstream load")
}

// TestConfig_ValidateRejectsDuplicateLabels verifies the uniqueness guard
// fires when two active paths would emit the same `path` label.
func TestConfig_ValidateRejectsDuplicateLabels(t *testing.T) {
	t.Parallel()

	cfg := &config{Paths: []pathConfig{
		{Pattern: "^/a$", Name: "shared", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}},
		{Pattern: "^/b$", Name: "shared", Methods: safeMethods(), Cache: cacheConfig{TTL: routeTestTTL}},
	}}
	cfg.initDefaults()
	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same metrics label")
	assert.Contains(t, err.Error(), `"shared"`)
}
