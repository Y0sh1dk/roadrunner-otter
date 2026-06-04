package otter

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_BuffersHeadersStatusBody(t *testing.T) {
	t.Parallel()

	rec := newRecorder(0)
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(http.StatusTeapot)
	_, err := rec.Write([]byte(`{"ok":true}`))
	require.NoError(t, err)

	snap := rec.snapshot()
	assert.Equal(t, http.StatusTeapot, snap.status)
	assert.Equal(t, "application/json", snap.header.Get("Content-Type"))
	assert.Equal(t, `{"ok":true}`, string(snap.body))
}

func TestRecorder_DefaultStatusIs200(t *testing.T) {
	t.Parallel()

	rec := newRecorder(0)
	_, err := rec.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.snapshot().status)
}

func TestRecorder_MaxBytesTrips(t *testing.T) {
	t.Parallel()

	rec := newRecorder(4)
	_, err := rec.Write([]byte("ab"))
	require.NoError(t, err)
	// This write puts us over the 4-byte cap.
	_, err = rec.Write([]byte("cdef"))
	require.NoError(t, err, "writes after the cap should be silently dropped, not error")
	assert.ErrorIs(t, rec.err, errResponseTooLarge)
}

func TestRecorder_SnapshotIsDeepCopy(t *testing.T) {
	t.Parallel()

	rec := newRecorder(0)
	rec.Header().Set("X-Foo", "v1")
	_, _ = rec.Write([]byte("body"))

	snap := rec.snapshot()
	// Mutating the recorder after snapshotting must not affect the snapshot.
	rec.Header().Set("X-Foo", "v2")
	_, _ = rec.Write([]byte("more"))

	assert.Equal(t, "v1", snap.header.Get("X-Foo"))
	assert.Equal(t, "body", string(snap.body))
}
