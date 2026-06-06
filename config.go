package otter

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	defaultMaxBodyBytes    = int64(8 << 20) // 8 MiB
	defaultCacheMaxEntries = 10_000
)

// https://datatracker.ietf.org/doc/html/rfc7231#section-6.1
var defaultCacheableStatuses = []int{
	http.StatusOK, http.StatusNonAuthoritativeInfo, http.StatusNoContent, http.StatusPartialContent,
	http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusPermanentRedirect,
	http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusRequestURITooLong,
	http.StatusNotImplemented,
}

var defaultCacheableMethods = []string{http.MethodGet, http.MethodHead}

type Config struct {
	Paths []PathConfig `mapstructure:"paths"`
}

type PathConfig struct {
	Pattern  string      `mapstructure:"pattern"`
	Name     string      `mapstructure:"name"` // optional metrics label; falls back to Pattern
	Disabled bool        `mapstructure:"disabled"`
	Methods  []string    `mapstructure:"methods"`
	Cache    CacheConfig `mapstructure:"cache"`
}

// Label returns the value used in Prometheus `path` labels. It is the
// user-supplied Name if set, otherwise the raw regex Pattern.
func (p PathConfig) Label() string {
	if p.Name != "" {
		return p.Name
	}

	return p.Pattern
}

type CacheConfig struct {
	// TTL is how long a cached response is served before re-fetching.
	// Required for non-disabled entries — there is no default, set it
	// explicitly per path. Use a YAML duration string ("5s", "1m", "100ms").
	TTL time.Duration `mapstructure:"ttl"`

	// MaxEntries is the per-path cache capacity. Entries beyond this
	// count are evicted by otter's W-TinyLFU policy. Default: 10000.
	MaxEntries int `mapstructure:"max_entries"`

	// MaxBodyBytes caps the response body size that the middleware is
	// willing to buffer and cache. Responses larger than this are
	// rejected with 502 instead of cached. Default: 8 MiB.
	MaxBodyBytes int64 `mapstructure:"max_body_bytes"`

	// Statuses is the set of upstream status codes that are eligible to
	// be cached. Anything outside this list bypasses storage and is
	// proxied through. Default: RFC 7231 §6.1 cacheable-by-default set.
	Statuses []int `mapstructure:"statuses"`

	Key KeyConfig `mapstructure:"key"`
}

type KeyConfig struct {
	IncludeHeaders []string `mapstructure:"include_headers"`
}

func (c *Config) InitDefaults() {
	for i := range c.Paths {
		p := &c.Paths[i]

		if p.Disabled {
			continue
		}

		// Sanitise methods
		for idx, method := range p.Methods {
			p.Methods[idx] = strings.ToUpper(strings.TrimSpace(method))
		}

		// Default methods
		if len(p.Methods) == 0 {
			p.Methods = slices.Clone(defaultCacheableMethods)
		}

		// Default max body
		if p.Cache.MaxBodyBytes == 0 {
			p.Cache.MaxBodyBytes = defaultMaxBodyBytes
		}

		// Default cache capacity
		if p.Cache.MaxEntries == 0 {
			p.Cache.MaxEntries = defaultCacheMaxEntries
		}

		if p.Cache.Statuses == nil {
			p.Cache.Statuses = slices.Clone(defaultCacheableStatuses)
		}
	}
}

func (c *Config) Validate() error {
	for i, p := range c.Paths {
		if err := validatePath(i, p); err != nil {
			return err
		}
	}

	return validateUniqueLabels(c.Paths)
}

func validateUniqueLabels(paths []PathConfig) error {
	seen := make(map[string]int, len(paths))

	for i, p := range paths {
		if p.Disabled {
			continue
		}

		label := p.Label()

		if prev, ok := seen[label]; ok {
			return fmt.Errorf(
				"otter: paths[%d] and paths[%d] resolve to the same metrics label %q; set a unique name",
				prev, i, label,
			)
		}

		seen[label] = i
	}

	return nil
}

// validatePath checks one PathConfig. Split out from Validate so each piece
// stays readable and the cyclomatic-complexity linter doesn't yell.
func validatePath(i int, p PathConfig) error {
	// Empty regex
	if strings.TrimSpace(p.Pattern) == "" {
		return fmt.Errorf("otter: paths[%d]: pattern is required", i)
	}

	// Valid regex
	if _, err := regexp.Compile(p.Pattern); err != nil {
		return fmt.Errorf("otter: paths[%d]: invalid pattern %q: %w", i, p.Pattern, err)
	}

	// Skip validation for disabled rules.
	if p.Disabled {
		return nil
	}

	if len(p.Methods) == 0 {
		return fmt.Errorf("otter: paths[%d]: methods is required (e.g. [\"GET\", \"HEAD\"])", i)
	}

	if slices.Contains(p.Methods, "") {
		return fmt.Errorf("otter: paths[%d]: empty method in list", i)
	}

	if p.Cache.MaxBodyBytes < 0 {
		return fmt.Errorf("otter: paths[%d]: cache.max_body_bytes must be >= 0, got %d", i, p.Cache.MaxBodyBytes)
	}

	return validatePathCache(i, p)
}

// validatePathCache checks the cache-related fields of a PathConfig.
func validatePathCache(i int, p PathConfig) error {
	if p.Cache.TTL <= 0 {
		return fmt.Errorf("otter: paths[%d]: cache.ttl is required and must be > 0 (e.g. \"5s\")", i)
	}

	if p.Cache.MaxEntries < 0 {
		return fmt.Errorf("otter: paths[%d]: cache.max_entries must be >= 0, got %d", i, p.Cache.MaxEntries)
	}

	if len(p.Cache.Statuses) == 0 {
		return fmt.Errorf("otter: paths[%d]: cache.statuses must contain at least one status code", i)
	}

	for _, code := range p.Cache.Statuses {
		if code < 100 || code > 599 {
			return fmt.Errorf("otter: paths[%d]: cache.statuses contains invalid HTTP status %d (must be 100-599)", i, code)
		}
	}

	return nil
}
