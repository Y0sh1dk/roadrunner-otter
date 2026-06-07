package otter

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
)

var errResponseTooLarge = errors.New("otter: upstream response exceeded max_body_bytes")

type snapshot struct {
	status int
	header http.Header
	body   []byte
}

// recorder is an http.ResponseWriter that buffers the upstream response.
type recorder struct {
	header      http.Header
	buf         bytes.Buffer
	status      int
	wroteHeader bool
	maxBytes    int64 // 0 = unlimited
	err         error
}

func newRecorder(maxBytes int64) *recorder {
	return &recorder{
		header:   make(http.Header),
		status:   http.StatusOK,
		maxBytes: maxBytes,
	}
}

func (r *recorder) Header() http.Header {
	return r.header
}

func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}

	r.status = status
	r.wroteHeader = true
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}

	if r.err != nil {
		// Once over the cap, swallow further writes so the upstream handler
		// can finish without erroring out — we stash the cap error on r.err
		// and the middleware surfaces it after ServeHTTP returns. Returning
		// (len(p), nil) here is intentional, not a missed error.
		return len(p), nil
	}

	if r.maxBytes > 0 && int64(r.buf.Len())+int64(len(p)) > r.maxBytes {
		r.err = errResponseTooLarge

		return len(p), nil
	}

	n, err := r.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("otter recorder: buffer write: %w", err)
	}

	return n, nil
}

func (r *recorder) snapshot() *snapshot {
	// Clone headers so the fanned-out copies can be mutated without races.
	h := make(http.Header, len(r.header))

	for k, v := range r.header {
		vv := make([]string, len(v))
		copy(vv, v)
		h[k] = vv
	}

	body := make([]byte, r.buf.Len())
	copy(body, r.buf.Bytes())

	return &snapshot{
		status: r.status,
		header: h,
		body:   body,
	}
}
