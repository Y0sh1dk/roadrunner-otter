package otter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyBuilder_MethodAndRequestURI(t *testing.T) {
	t.Parallel()

	kb := newKeyBuilder(keyConfig{})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/foo?bar=1", nil)
	got := kb.build(r)
	assert.Equal(t, "GET /foo?bar=1", got)
}

func TestKeyBuilder_QueryStringIncluded(t *testing.T) {
	t.Parallel()

	// /feed?page=1 and /feed?page=2 MUST NOT collide — the query is part
	// of the cache key. This is the headline safety property.
	kb := newKeyBuilder(keyConfig{})

	r1 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/feed?page=1", nil)
	r2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/feed?page=2", nil)
	assert.NotEqual(t, kb.build(r1), kb.build(r2))
}

func TestKeyBuilder_MethodDistinguishes(t *testing.T) {
	t.Parallel()

	kb := newKeyBuilder(keyConfig{})

	get := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	head := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/x", nil)
	assert.NotEqual(t, kb.build(get), kb.build(head))
}

func TestKeyBuilder_IncludeHeaders(t *testing.T) {
	t.Parallel()

	kb := newKeyBuilder(keyConfig{
		IncludeHeaders: []string{"authorization", "Accept-Language"},
	})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
	r.Header.Set("Authorization", "Bearer alice")
	r.Header.Set("Accept-Language", "en-US")
	keyAlice := kb.build(r)

	r.Header.Set("Authorization", "Bearer bob")
	keyBob := kb.build(r)

	assert.NotEqual(t, keyAlice, keyBob, "different auth headers must yield different keys")
	assert.Contains(t, keyAlice, "Authorization=Bearer alice")
	assert.Contains(t, keyAlice, "Accept-Language=en-US")
}

func TestKeyBuilder_HeadersAreCanonicalized(t *testing.T) {
	t.Parallel()

	// Configuring with a lowercase header name must still match the
	// canonicalized form Go puts in r.Header.
	kb := newKeyBuilder(keyConfig{IncludeHeaders: []string{"x-tenant-id"}})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	r.Header.Set("X-Tenant-Id", "acme")
	got := kb.build(r)
	assert.Contains(t, got, "X-Tenant-Id=acme")
}

func TestKeyBuilder_MultiValueHeadersJoined(t *testing.T) {
	t.Parallel()

	kb := newKeyBuilder(keyConfig{IncludeHeaders: []string{"X-Forwarded-For"}})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	r.Header.Add("X-Forwarded-For", "10.0.0.1")
	r.Header.Add("X-Forwarded-For", "10.0.0.2")
	got := kb.build(r)
	assert.Contains(t, got, "X-Forwarded-For=10.0.0.1,10.0.0.2")
}

func TestKeyBuilder_MissingHeaderProducesEmptyValue(t *testing.T) {
	t.Parallel()

	// A header named in IncludeHeaders but absent from the request must
	// still be in the key (with empty value) so its presence/absence is
	// itself part of the cache identity.
	kb := newKeyBuilder(keyConfig{IncludeHeaders: []string{"X-Trace"}})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	got := kb.build(r)
	assert.Contains(t, got, "X-Trace=",
		"absent header should still contribute %q to the key: %q", "X-Trace=", got)
}
