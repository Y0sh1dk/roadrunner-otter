package otter

import (
	"net/http"
	"strings"
)

const (
	unitSeparator = '\x1f'
)

type keyBuilder struct {
	includeHeaders []string // canonicalized header names
}

func newKeyBuilder(cfg keyConfig) *keyBuilder {
	canon := make([]string, len(cfg.IncludeHeaders))
	for i, h := range cfg.IncludeHeaders {
		canon[i] = http.CanonicalHeaderKey(strings.TrimSpace(h))
	}

	return &keyBuilder{includeHeaders: canon}
}

func (kb *keyBuilder) build(r *http.Request) string {
	uri := r.URL.RequestURI()

	// No headers to include.
	if len(kb.includeHeaders) == 0 {
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
		sb.WriteByte(unitSeparator)
		sb.WriteString(h)
		sb.WriteByte('=')

		if vals := r.Header.Values(h); len(vals) > 0 {
			sb.WriteString(strings.Join(vals, ","))
		}
	}

	return sb.String()
}
