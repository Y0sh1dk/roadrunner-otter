package otter

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// nullHandler responds 200 with no body. It's the cheapest possible upstream
// and lets benchmarks measure per-request middleware overhead in isolation.
var nullHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// benchPlugin builds a Plugin with a catch-all "^.*$" path so the
// middleware engages for every benchmarked request.
func benchPlugin(b *testing.B) *Plugin {
	b.Helper()

	p := &Plugin{}

	cfg := &fakeConfigurer{section: PluginName, cfg: &Config{
		Paths: []PathConfig{{
			Pattern:  "^.*$",
			Methods:  []string{"GET", "HEAD"},
			CacheTTL: 5 * time.Minute,
		}},
	}}

	err := p.Init(cfg, nil)
	if err != nil {
		b.Fatal(err)
	}

	return p
}

// discardWriter is an http.ResponseWriter that drops everything. Cheaper than
// httptest.NewRecorder in tight benchmark loops (no buffer allocation, no
// header map reuse cost).
type discardWriter struct{ h http.Header }

func newDiscardWriter() *discardWriter               { return &discardWriter{h: make(http.Header)} }
func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardWriter) WriteHeader(int)             {}

// BenchmarkBypass measures overhead when the method is NOT in the coalescing
// set. Should be a handful of ns — route match + slices.Contains for the
// method check + a function call.
func BenchmarkBypass(b *testing.B) {
	p := benchPlugin(b)
	h := p.Middleware(nullHandler)
	req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/x", nil) // POST → bypassed
	w := newDiscardWriter()

	b.ReportAllocs()

	for b.Loop() {
		h.ServeHTTP(w, req)
	}
}

// BenchmarkBaselineHandler is the control: nullHandler with no middleware.
// Compare against BenchmarkColdSerial to read off the per-request cost the
// middleware adds.
func BenchmarkBaselineHandler(b *testing.B) {
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/x", nil)
	w := newDiscardWriter()

	b.ReportAllocs()

	for b.Loop() {
		nullHandler.ServeHTTP(w, req)
	}
}

// BenchmarkColdSerial measures the no-contention path: each iteration uses a
// distinct key so the cache always misses and the recorder gets
// allocated, used, and discarded once per call. This is the worst case for
// middleware overhead — production traffic with no hot keys looks like this.
func BenchmarkColdSerial(b *testing.B) {
	p := benchPlugin(b)
	h := p.Middleware(nullHandler)

	// Pre-build requests with distinct paths to avoid measuring URL parsing.
	reqs := make([]*http.Request, 1024)
	for i := range reqs {
		reqs[i] = httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/p/"+strconv.Itoa(i), nil)
	}

	w := newDiscardWriter()

	b.ReportAllocs()

	i := 0
	for b.Loop() {
		h.ServeHTTP(w, reqs[i&1023])

		i++
	}
}

// BenchmarkParallelCoalesce measures throughput when many goroutines hit the
// same key. With a populated cache this is mostly the cache-hit hot path
// (route match + key build + otter Get → cached snapshot replay).
func BenchmarkParallelCoalesce(b *testing.B) {
	p := benchPlugin(b)
	h := p.Middleware(nullHandler)
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/hot", nil)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newDiscardWriter()
		for pb.Next() {
			h.ServeHTTP(w, req)
		}
	})
}

// BenchmarkParallelCoalesce_SlowUpstream measures coalescing under genuine
// contention: the upstream blocks long enough that concurrent callers pile
// up. This is the workload the middleware is *designed* for, so we expect
// near-flat throughput regardless of GOMAXPROCS.
func BenchmarkParallelCoalesce_SlowUpstream(b *testing.B) {
	var calls atomic.Int64

	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Busy-spin a hair so we measure actual coalescing, not goroutine
		// scheduling. Real-world upstreams take milliseconds.
		for i := range 1000 {
			_ = i
		}

		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	p := benchPlugin(b)
	h := p.Middleware(slow)
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/hot", nil)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newDiscardWriter()
		for pb.Next() {
			h.ServeHTTP(w, req)
		}
	})
	b.ReportMetric(float64(calls.Load())/float64(b.N), "upstream/op")
}

// BenchmarkKeyBuilder_Default measures rendering cost of the default
// (method + RequestURI) shape — what most requests look like.
func BenchmarkKeyBuilder_Default(b *testing.B) {
	kb := newKeyBuilder(KeyConfig{})
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/v1/users/12345?include=profile", nil)

	b.ReportAllocs()

	for b.Loop() {
		_ = kb.build(r)
	}
}

// BenchmarkKeyBuilder_WithHeaders measures the more expensive path: key
// derived with two included headers. This is the realistic shape of a
// per-tenant cache key.
func BenchmarkKeyBuilder_WithHeaders(b *testing.B) {
	kb := newKeyBuilder(KeyConfig{
		IncludeHeaders: []string{"Authorization", "Accept-Language"},
	})
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/v1/me", nil)
	r.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.short.signature")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")

	b.ReportAllocs()

	for b.Loop() {
		_ = kb.build(r)
	}
}

// BenchmarkRecorderWrite measures the write throughput of the buffering
// recorder under the default 8 MiB cap.
func BenchmarkRecorderWrite(b *testing.B) {
	payload := make([]byte, 1024)
	b.SetBytes(int64(len(payload)))

	b.ReportAllocs()

	for b.Loop() {
		rec := newRecorder(defaultMaxBodyBytes)
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(payload)
		_ = rec.snapshot()
	}
}

// BenchmarkRecorderWrite_Capped measures the path where writes overflow the
// cap. Once tripped, further writes are dropped — this should be cheap.
func BenchmarkRecorderWrite_Capped(b *testing.B) {
	payload := make([]byte, 1024)

	b.ReportAllocs()

	for b.Loop() {
		rec := newRecorder(512) // cap below payload size — every write trips it
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(payload)
	}
}

// BenchmarkRouteLookup measures the per-request cost of the path-match
// lookup. With 20 patterns and a worst-case request that matches the last
// one, this is what users with deep `paths:` configs pay every request.
func BenchmarkRouteLookup(b *testing.B) {
	paths := make([]PathConfig, 20)
	for i := range paths {
		paths[i] = PathConfig{
			Pattern:  "^/api/v" + strconv.Itoa(i) + "/.*",
			Methods:  []string{"GET", "HEAD"},
			CacheTTL: 5 * time.Minute,
		}
	}

	cfg := &Config{Paths: paths}
	cfg.InitDefaults()

	if err := cfg.Validate(); err != nil {
		b.Fatal(err)
	}

	rt, err := buildRouteTable(cfg)
	if err != nil {
		b.Fatal(err)
	}

	// Match the last pattern → worst case (full table scan).
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/v19/users", nil)

	b.ReportAllocs()

	for b.Loop() {
		_, _ = rt.match(req)
	}
}

// BenchmarkRouteLookup_NoPaths measures the cost when the paths list is
// empty — should be essentially free (one slice-length check + return).
// This is also the "middleware effectively disabled" path.
func BenchmarkRouteLookup_NoPaths(b *testing.B) {
	cfg := &Config{}
	cfg.InitDefaults()

	rt, err := buildRouteTable(cfg)
	if err != nil {
		b.Fatal(err)
	}

	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/anything", nil)

	b.ReportAllocs()

	for b.Loop() {
		_, _ = rt.match(req)
	}
}
