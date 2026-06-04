package otter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/maypok86/otter/v2"
)

const PluginName = "otter"

// Configurer provides access to the application configuration.
type Configurer interface {
	// UnmarshalKey takes a single key and unmarshal it into a Struct.
	UnmarshalKey(name string, out any) error
	// Has checks if a config section exists.
	Has(name string) bool
}

// Logger is the main logger interface to provide a named (per-plugin) logger.
type Logger interface {
	NamedLogger(name string) *slog.Logger
}

type Plugin struct {
	config *Config
	log    *slog.Logger
	routes *routeTable
}

func (p *Plugin) Init(cfg Configurer, log Logger) error {
	p.config = &Config{}

	if cfg != nil && cfg.Has(PluginName) { // .rr.yaml has `otter:`
		err := cfg.UnmarshalKey(PluginName, p.config)
		if err != nil {
			return fmt.Errorf("otter: unmarshal config: %w", err)
		}
	}

	p.config.InitDefaults()

	if err := p.config.Validate(); err != nil {
		return err
	}

	rt, err := buildRouteTable(p.config)
	if err != nil {
		return err
	}

	p.routes = rt

	if log != nil {
		p.log = log.NamedLogger(PluginName)
	} else {
		p.log = slog.New(slog.DiscardHandler)
	}

	return nil
}

func (p *Plugin) Name() string {
	return PluginName
}

func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, enabled := p.routes.match(r)
		if !enabled || !route.acceptsMethod(r.Method) {
			next.ServeHTTP(w, r)

			return
		}

		key := route.kb.build(r)

		snapshot, err := route.cache.Get(r.Context(), key, otter.LoaderFunc[string, *snapshot](
			func(_ context.Context, _ string) (*snapshot, error) {
				rec := newRecorder(route.maxBodyBytes)
				next.ServeHTTP(rec, r)

				if rec.err != nil {
					return nil, rec.err
				}

				s := rec.snapshot()
				if !route.isCacheable(s.status) {
					return s, errSkipCache
				}

				return s, nil
			},
		))

		switch {
		case errors.Is(err, errSkipCache):
			writeSnapshot(w, snapshot)
		case errors.Is(err, errResponseTooLarge):
			p.log.Warn("response exceeded max_body_bytes; cannot cache", slog.String("key", key))
			http.Error(w, "upstream response too large to cache", http.StatusBadGateway)
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			writeSnapshot(w, snapshot)
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
