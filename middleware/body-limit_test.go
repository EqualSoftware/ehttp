package middleware_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func bPost(body io.Reader, contentLength int64) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/x", body)
	req.ContentLength = contentLength

	return req
}

func TestBodyLimitUnits(t *testing.T) {
	t.Parallel()

	const basis = "BodyLimit doc: 4x or 4xB"

	tests := []struct {
		limit string
		size  int64 // largest accepted Content-Length
	}{
		{"10", 10},
		{"1K", 1024},
		{"1KB", 1024},
		{"1k", 1024},
		{"2M", 2 << 20},
		{"2MB", 2 << 20},
		{"1G", 1 << 30},
		{"1.5K", 1536},
	}

	for _, tt := range tests {
		t.Run(tt.limit, func(t *testing.T) {
			t.Parallel()

			h := fixtures.PostHandler(fixtures.OK, middleware.BodyLimit(tt.limit))

			at := ehttptest.Do(h, bPost(strings.NewReader(""), tt.size))
			over := ehttptest.Do(h, bPost(strings.NewReader(""), tt.size+1))

			ehttptest.Eq(t, basis+" (at limit)", at.Code, 200)
			ehttptest.Eq(t, "RFC 9110 §15.5.14 (over limit)", over.Code, 413)
		})
	}
}

func TestBodyLimitContract(t *testing.T) {
	t.Parallel()

	t.Run("413 without calling next or reading the body", func(t *testing.T) {
		t.Parallel()

		called := false
		cr := &bCountingReader{r: strings.NewReader(strings.Repeat("a", 100))}
		h := fixtures.PostHandler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			called = true
			res.String("ok")
		}, middleware.BodyLimit("10"))

		rec := ehttptest.Do(h, bPost(cr, 100))

		ehttptest.Eq(t, "RFC 9110 §15.5.14", rec.Code, 413)
		ehttptest.True(t, "BodyLimit contract", !called, "next was called for an oversized request")
		ehttptest.Eq(t, "C5 (413 without reading all)", cr.n, 0)
	})

	t.Run("no body passes through", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.PostHandler(fixtures.OK, middleware.BodyLimit("10")), bPost(http.NoBody, 0))

		ehttptest.Eq(t, "BodyLimit contract", rec.Code, 200)
		ehttptest.Eq(t, "BodyLimit contract", rec.Body.String(), "ok")
	})

	t.Run("unknown length is capped while reading", func(t *testing.T) {
		t.Parallel()

		var readErr error
		var n int
		h := fixtures.PostHandler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			b, err := io.ReadAll(req.Request.Body)
			readErr, n = err, len(b)
			res.SendStatus(200)
		}, middleware.BodyLimit("10"))

		ehttptest.Do(h, bPost(io.NopCloser(strings.NewReader(strings.Repeat("a", 100))), -1))

		var mbe *http.MaxBytesError
		ehttptest.True(t, "net/http MaxBytesReader", readErr != nil && errorsAs(readErr, &mbe),
			"read error = %v, want *http.MaxBytesError", readErr)
		ehttptest.True(t, "net/http MaxBytesReader", n <= 10, "read %d bytes, limit is 10", n)
	})

	t.Run("body within limit is readable", func(t *testing.T) {
		t.Parallel()

		var got string
		h := fixtures.PostHandler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			b, _ := io.ReadAll(req.Request.Body)
			got = string(b)
			res.SendStatus(200)
		}, middleware.BodyLimit("10"))

		rec := ehttptest.Do(h, bPost(strings.NewReader("hello"), 5))

		ehttptest.Eq(t, "BodyLimit contract", rec.Code, 200)
		ehttptest.Eq(t, "BodyLimit contract", got, "hello")
	})
}

func TestBodyLimitInvalidValue(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"4 KB", "abc", "", "10XB", "K"} {
		t.Run("invalid "+v, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "C5", func(t testing.TB) {
				panicked := func() (p bool) {
					defer func() { p = recover() != nil }()
					middleware.BodyLimit(v)

					return false
				}()

				ehttptest.True(t, "C5", panicked, "BodyLimit(%q) accepted an invalid limit at boot", v)
			})
		})
	}
}

type bCountingReader struct {
	r io.Reader
	n int
}

func (c *bCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n

	return n, err
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
