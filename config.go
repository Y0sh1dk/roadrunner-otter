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

type Config struct {
	Paths []PathConfig `mapstructure:"paths"`
}

type PathConfig struct {
	Pattern      string    `mapstructure:"pattern"`
	Disabled     bool      `mapstructure:"disabled"`
	Methods      []string  `mapstructure:"methods"`
	Key          KeyConfig `mapstructure:"key"`
	MaxBodyBytes int64     `mapstructure:"max_body_bytes"`

	// CacheTTL is how long a cached response is served before re-fetching.
	// Required for non-disabled entries — there is no default, set it
	// explicitly per path. Use a YAML duration string ("5s", "1m", "100ms").
	CacheTTL time.Duration `mapstructure:"cache_ttl"`

	// CacheMaxEntries is the per-path cache capacity. Entries beyond this
	// count are evicted by otter's W-TinyLFU policy. Default: 10000.
	CacheMaxEntries int `mapstructure:"cache_max_entries"`

	CacheStatuses []int `mapstructure:"cache_statuses"`
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

		// Default max body
		if p.MaxBodyBytes == 0 {
			p.MaxBodyBytes = defaultMaxBodyBytes
		}

		// Default cache capacity. TTL has no default — required, validated.
		if p.CacheMaxEntries == 0 {
			p.CacheMaxEntries = defaultCacheMaxEntries
		}

		if p.CacheStatuses == nil {
			p.CacheStatuses = slices.Clone(defaultCacheableStatuses)
		}
	}
}

func (c *Config) Validate() error {
	for i, p := range c.Paths {
		if err := validatePath(i, p); err != nil {
			return err
		}
	}

	return nil
}

// validatePath checks one PathConfig. Split out from Validate so each piece
// stays readable and the cyclomatic-complexity linter doesn't yell.
func validatePath(i int, p PathConfig) error {
	if strings.TrimSpace(p.Pattern) == "" {
		return fmt.Errorf("otter: paths[%d]: pattern is required", i)
	}

	if _, err := regexp.Compile(p.Pattern); err != nil {
		return fmt.Errorf("otter: paths[%d]: invalid pattern %q: %w", i, p.Pattern, err)
	}

	if p.Disabled {
		return nil
	}

	if len(p.Methods) == 0 {
		return fmt.Errorf("otter: paths[%d]: methods is required (no default applied)", i)
	}

	if slices.Contains(p.Methods, "") {
		return fmt.Errorf("otter: paths[%d]: empty method in methods list", i)
	}

	if p.MaxBodyBytes < 0 {
		return fmt.Errorf("otter: paths[%d]: max_body_bytes must be >= 0, got %d", i, p.MaxBodyBytes)
	}

	return validatePathCache(i, p)
}

// validatePathCache checks the cache-related fields of a PathConfig.
func validatePathCache(i int, p PathConfig) error {
	if p.CacheTTL <= 0 {
		return fmt.Errorf("otter: paths[%d]: cache_ttl is required and must be > 0 (e.g. \"5s\")", i)
	}

	if p.CacheMaxEntries < 0 {
		return fmt.Errorf("otter: paths[%d]: cache_max_entries must be >= 0, got %d", i, p.CacheMaxEntries)
	}

	if len(p.CacheStatuses) == 0 {
		return fmt.Errorf("otter: paths[%d]: cache_statuses must contain at least one status code", i)
	}

	for _, code := range p.CacheStatuses {
		if code < 100 || code > 599 {
			return fmt.Errorf("otter: paths[%d]: cache_statuses contains invalid HTTP status %d (must be 100-599)", i, code)
		}
	}

	return nil
}
