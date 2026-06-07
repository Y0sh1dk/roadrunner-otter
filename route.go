package otter

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/maypok86/otter/v2"
	"github.com/maypok86/otter/v2/stats"
)

type routeTable struct {
	routes []*compiledRoute
}

type compiledRoute struct {
	regex    *regexp.Regexp
	disabled bool
	label    string // metrics label (Name if set, else Pattern)

	methods       []string
	kb            *keyBuilder
	maxBodyBytes  int64
	cacheStatuses []int
	cache         *otter.Cache[string, *snapshot]
}

func (r *compiledRoute) isCacheable(s *snapshot) bool {
	// Only condition is response status currently.
	return slices.Contains(r.cacheStatuses, s.status)
}

func (t *routeTable) match(req *http.Request) (*compiledRoute, bool) {
	for _, route := range t.routes {
		// Matches regex.
		if route.regex.MatchString(req.URL.Path) {
			// Route is disabled.
			if route.disabled {
				return nil, false
			}

			// Request method is cachable.
			if slices.Contains(route.methods, req.Method) {
				return route, true
			}
		}
	}

	// No match or route disabled.
	return nil, false
}

func buildRouteTable(cfg *config) (*routeTable, error) {
	routeTable := &routeTable{}

	// Build compiled routes from config.
	for index, path := range cfg.Paths {
		regex, err := regexp.Compile(path.Pattern)
		if err != nil {
			return nil, fmt.Errorf("otter: paths[%d]: invalid pattern %q: %w", index, path.Pattern, err)
		}

		route := &compiledRoute{
			regex:    regex,
			disabled: path.Disabled,
			label:    path.label(),
		}

		if !path.Disabled {
			route.methods = path.Methods
			route.kb = newKeyBuilder(path.Cache.KeyConfig)
			route.maxBodyBytes = path.Cache.MaxBodyBytes
			route.cacheStatuses = path.Cache.Statuses
			route.cache = otter.Must(&otter.Options[string, *snapshot]{
				MaximumSize:      path.Cache.MaxEntries,
				ExpiryCalculator: otter.ExpiryWriting[string, *snapshot](path.Cache.TTL),
				StatsRecorder:    stats.NewCounter(),
			})
		}

		// Append to route table in same order as defined, no ordering.
		routeTable.routes = append(routeTable.routes, route)
	}

	return routeTable, nil
}
