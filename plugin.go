package otter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/maypok86/otter/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
)

const pluginName = "otter"

type configurer interface {
	UnmarshalKey(name string, out any) error
	Has(name string) bool
}

type logger interface {
	NamedLogger(name string) *zap.Logger
}

type Plugin struct {
	config *config
	log    *slog.Logger
	routes *routeTable
}

func (p *Plugin) Init(cfg configurer, log logger) error {
	p.config = &config{}

	// .rr.yaml has `otter:` top-level key.
	if cfg != nil && cfg.Has(pluginName) {
		err := cfg.UnmarshalKey(pluginName, p.config)
		if err != nil {
			return fmt.Errorf("otter: unmarshal config: %w", err)
		}
	}

	// Set defaults for any missing config values.
	p.config.initDefaults()

	// Validate config values, error if invalid.
	if err := p.config.validate(); err != nil {
		return err
	}

	// Build route table from config.
	rt, err := buildRouteTable(p.config)
	if err != nil {
		return err
	}

	p.routes = rt

	// Initialize logger. If no logger provided, use a no-op logger.
	if log != nil {
		p.log = slog.New(zapslog.NewHandler(log.NamedLogger(pluginName).Core()))
	} else {
		p.log = slog.New(slog.DiscardHandler)
	}

	return nil
}

func (p *Plugin) Name() string {
	return pluginName
}

func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, enabled := p.routes.match(r)
		// No match or route disabled
		if !enabled {
			next.ServeHTTP(w, r)

			return
		}

		// Create cache key based on route cache config.
		key := route.kb.build(r)

		p.log.Debug("cache key built",
			slog.String("route", route.label),
			slog.String("key", key),
		)

		responseSnapshot, err := route.cache.Get(r.Context(), key, otter.LoaderFunc[string, *snapshot](
			func(_ context.Context, _ string) (*snapshot, error) {
				// Create recorder to capture upstream response
				recorder := newRecorder(route.maxBodyBytes)

				// Call upstream handler
				next.ServeHTTP(recorder, r)

				// Recorder can error if response breaks constraints.
				if recorder.err != nil {
					return nil, recorder.err
				}

				// Create snapshot from recorder response to store in cache.
				s := recorder.snapshot()

				// Drop configured response headers before storing so they don't leak to subsequent cache hits.
				for _, header := range route.stripHeaders {
					s.header.Del(header)
				}

				// Not cachable.
				if !route.isCacheable(s) {
					return s, errSkipCache
				}

				// Cachable.
				return s, nil
			},
		))

		switch {
		case errors.Is(err, errSkipCache):
			// Response not cachable, return upstream response without caching.
			writeSnapshot(w, responseSnapshot)
		case errors.Is(err, errResponseTooLarge):
			// Upstream response exceeded max body size, log and return 502.
			p.log.Warn("response exceeded max_body_bytes; cannot cache", slog.String("key", key))
			http.Error(w, "upstream response too large to cache", http.StatusBadGateway)
		case err != nil:
			// Unexpected error, return 502.
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			// Cache hit, return cached response.
			writeSnapshot(w, responseSnapshot)
		}
	})
}

var errSkipCache = errors.New("otter: response status not in cacheable set")

func writeSnapshot(w http.ResponseWriter, snapshot *snapshot) {
	dst := w.Header()
	for k, v := range snapshot.header {
		dst[k] = append(dst[k][:0:0], v...)
	}

	w.WriteHeader(snapshot.status)

	if len(snapshot.body) > 0 {
		_, _ = w.Write(snapshot.body)
	}
}
