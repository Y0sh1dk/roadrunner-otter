package otter

import (
	"github.com/prometheus/client_golang/prometheus"
)

const metricsNamespace = "otter"

// pathLabel is the single label every otter metric carries. Its value is the
// user-supplied PathConfig.Name (when set) or the regex Pattern.
const pathLabel = "path"

var (
	hitsDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_hits_total",
		"Cache lookups that returned a stored response without invoking upstream.",
		[]string{pathLabel}, nil,
	)
	missesDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_misses_total",
		"Cache lookups that did not find a stored response.",
		[]string{pathLabel}, nil,
	)
	evictionsDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_evictions_total",
		"Entries evicted by the cache's W-TinyLFU policy (excludes manual deletions).",
		[]string{pathLabel}, nil,
	)
	loadSuccessesDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_load_successes_total",
		"Successful upstream loads triggered by a cache miss.",
		[]string{pathLabel}, nil,
	)
	loadFailuresDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_load_failures_total",
		"Upstream loads that returned an error (e.g. response exceeded max_body_bytes).",
		[]string{pathLabel}, nil,
	)
	sizeDesc = prometheus.NewDesc(
		metricsNamespace+"_cache_size",
		"Approximate number of entries currently held in the cache.",
		[]string{pathLabel}, nil,
	)
)

// MetricsCollector satisfies roadrunner-server/metrics' StatProvider so the
// RR metrics plugin auto-registers our collector with its Prometheus
// registry. Returned at plugin Collects time; called on every scrape.
func (p *Plugin) MetricsCollector() []prometheus.Collector {
	return []prometheus.Collector{&otterCollector{routes: p.routes}}
}

// otterCollector emits per-path samples sourced from each route's
// otter.Cache.Stats() snapshot and EstimatedSize() on every scrape.
type otterCollector struct {
	routes *routeTable
}

func (c *otterCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		hitsDesc, missesDesc, evictionsDesc,
		loadSuccessesDesc, loadFailuresDesc, sizeDesc,
	} {
		ch <- d
	}
}

func (c *otterCollector) Collect(ch chan<- prometheus.Metric) {
	if c.routes == nil {
		return
	}

	for _, r := range c.routes.routes {
		if r.disabled || r.cache == nil {
			continue
		}

		s := r.cache.Stats()
		emit := func(d *prometheus.Desc, t prometheus.ValueType, v float64) {
			ch <- prometheus.MustNewConstMetric(d, t, v, r.label)
		}

		emit(hitsDesc, prometheus.CounterValue, float64(s.Hits))
		emit(missesDesc, prometheus.CounterValue, float64(s.Misses))
		emit(evictionsDesc, prometheus.CounterValue, float64(s.Evictions))
		emit(loadSuccessesDesc, prometheus.CounterValue, float64(s.LoadSuccesses))
		emit(loadFailuresDesc, prometheus.CounterValue, float64(s.LoadFailures))
		emit(sizeDesc, prometheus.GaugeValue, float64(r.cache.EstimatedSize()))
	}
}
