package otter

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/maypok86/otter/v2"
)

type compiledRoute struct {
	re       *regexp.Regexp
	disabled bool

	methods       []string
	kb            *keyBuilder
	maxBodyBytes  int64
	cacheStatuses []int
	cache         *otter.Cache[string, *snapshot]
}

// isCacheable returns true if a response with the given status code should
// be stored in the cache. Called once per upstream completion.
func (r *compiledRoute) isCacheable(status int) bool {
	return slices.Contains(r.cacheStatuses, status)
}

func (r *compiledRoute) acceptsMethod(method string) bool {
	return slices.Contains(r.methods, method)
}

type routeTable struct {
	routes []*compiledRoute
}

func (t *routeTable) match(req *http.Request) (*compiledRoute, bool) {
	for _, rt := range t.routes {
		if rt.re.MatchString(req.URL.Path) {
			if rt.disabled {
				return nil, false
			}

			return rt, true
		}
	}

	return nil, false
}

func buildRouteTable(cfg *Config) (*routeTable, error) {
	routeTable := &routeTable{}

	for index, path := range cfg.Paths {
		re, err := regexp.Compile(path.Pattern)
		if err != nil {
			return nil, fmt.Errorf("otter: paths[%d]: invalid pattern %q: %w", index, path.Pattern, err)
		}

		route := &compiledRoute{
			re:       re,
			disabled: path.Disabled,
		}

		if !path.Disabled {
			route.methods = path.Methods
			route.kb = newKeyBuilder(path.Cache.Key)
			route.maxBodyBytes = path.Cache.MaxBodyBytes
			route.cacheStatuses = path.Cache.Statuses
			route.cache = otter.Must(&otter.Options[string, *snapshot]{
				MaximumSize:      path.Cache.MaxEntries,
				ExpiryCalculator: otter.ExpiryWriting[string, *snapshot](path.Cache.TTL),
			})
		}

		routeTable.routes = append(routeTable.routes, route)
	}

	return routeTable, nil
}
