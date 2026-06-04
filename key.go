package otter

import (
	"net/http"
	"strings"
)

// keyBuilder renders a per-request cache key as
//
//	METHOD RequestURI [\x1fHeader=value ...]
//
// The key is always (method, path-with-query, included-headers). There is
// no user-supplied template — this keeps the key derivation predictable
// and prevents accidental cache collisions across requests that look
// similar but shouldn't share a response.
type keyBuilder struct {
	includeHeaders []string // canonicalized header names
}

func newKeyBuilder(cfg KeyConfig) *keyBuilder {
	canon := make([]string, len(cfg.IncludeHeaders))
	for i, h := range cfg.IncludeHeaders {
		canon[i] = http.CanonicalHeaderKey(strings.TrimSpace(h))
	}

	return &keyBuilder{includeHeaders: canon}
}

func (kb *keyBuilder) build(r *http.Request) string {
	// RequestURI is path + "?query" (or just path when there's no query),
	// which is what most users mean by "path" colloquially. Using just
	// URL.Path would cause /feed?page=1 and /feed?page=2 to collide.
	uri := r.URL.RequestURI()

	if len(kb.includeHeaders) == 0 {
		// Fast path: most common shape (no header-based keying).
		var sb strings.Builder

		sb.Grow(len(r.Method) + 1 + len(uri))
		sb.WriteString(r.Method)
		sb.WriteByte(' ')
		sb.WriteString(uri)

		return sb.String()
	}

	var sb strings.Builder
	// Approximate pre-size: method + space + uri + ~32 bytes per header.
	sb.Grow(len(r.Method) + 1 + len(uri) + 32*len(kb.includeHeaders))
	sb.WriteString(r.Method)
	sb.WriteByte(' ')
	sb.WriteString(uri)

	for _, h := range kb.includeHeaders {
		sb.WriteByte('\x1f') // unit separator — unlikely to appear in header values
		sb.WriteString(h)
		sb.WriteByte('=')

		if vals := r.Header.Values(h); len(vals) > 0 {
			sb.WriteString(strings.Join(vals, ","))
		}
	}

	return sb.String()
}
