// Package otter provides a RoadRunner HTTP middleware that caches and
// coalesces concurrent identical requests using
// github.com/maypok86/otter/v2. Cache hits skip the upstream entirely;
// concurrent cache misses for the same key are deduplicated into a single
// loader call (stampede protection).
package otter
