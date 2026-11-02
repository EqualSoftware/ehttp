package middleware_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		ehttptest.Eq(t, "BodyLimit contract + RFC 9110 §15.5.14 (reject on Content-Length, body unread)", cr.n, 0)
	})

	t.Run("no body passes through", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.PostHandler(fixtures.OK, middleware.BodyLimit("10")), bPost(http.NoBody, 0))

		ehttptest.Eq(t, "BodyLimit contract", rec.Code, 200)
		ehttptest.Eq(t, "BodyLimit contract", rec.Body.String(), "ok")
	})

	// For a body of unknown length (chunked) the middleware cannot reject up
	// front. Its contract: the handler's read fails with ErrRequestBodyTooLarge
	// and stops at the limit. The 413 is then the handler's job: it propagates
	// the error (RFC 9110 §15.5.14). BodyLimit does not write 413 itself here.
	t.Run("unknown length: handler read fails with ErrRequestBodyTooLarge and stops at the limit", func(t *testing.T) {
		t.Parallel()

		var readErr error
		src := &bCountingReader{r: strings.NewReader(strings.Repeat("a", 100))}
		h := fixtures.PostHandler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			_, readErr = req.Body()
			if readErr != nil {
				res.SendStatus(413) // a handler that propagates the error
				return
			}

			res.SendStatus(200)
		}, middleware.BodyLimit("10"))

		rec := ehttptest.Do(h, bPost(io.NopCloser(src), -1))

		ehttptest.True(t, "ErrRequestBodyTooLarge contract", errors.Is(readErr, ehttp.ErrRequestBodyTooLarge),
			"read error = %v, want ErrRequestBodyTooLarge", readErr)
		ehttptest.True(t, "net/http MaxBytesReader (reads at most limit+1)", src.n <= 11, "source read %d bytes, limit is 10", src.n)
		ehttptest.Eq(t, "RFC 9110 §15.5.14 (handler propagates the error)", rec.Code, 413)
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

			ehttptest.KnownBug(t, "body-limit-accepts-invalid-value", func(t testing.TB) {
				panicked := func() (p bool) {
					defer func() { p = recover() != nil }()
					middleware.BodyLimit(v)

					return false
				}()

				ehttptest.True(t, "an invalid limit panics at boot", panicked, "BodyLimit(%q) accepted an invalid limit at boot", v)
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

// F1: on overflow MaxBytesReader asks the real writer for "Connection: close".
// BodyLimit must hand it the real writer, also under an interceptor.
func TestBodyLimitClosesConnectionOnOverflow(t *testing.T) {
	t.Parallel()

	for name, mws := range map[string][]ehttp.MiddlewareHandlerFunc{
		"plain":         {middleware.BodyLimit("10")},
		"inside etag":   {middleware.WeakEtag(), middleware.BodyLimit("10")},
		"inside errcch": {middleware.ErrorCache(time.Minute), middleware.BodyLimit("10")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := ehttp.NewServer()
			srv.POST("/x", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
				if _, err := io.ReadAll(req.Request.Body); err != nil {
					res.SendStatus(http.StatusRequestEntityTooLarge)
					return
				}

				res.SendStatus(http.StatusOK)
			}, mws...)

			ts := fixtures.Start(t, srv)
			body := io.NopCloser(strings.NewReader(strings.Repeat("a", 100))) // chunked
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/x", body)
			ehttptest.Eq(t, "request", err, nil)

			resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: false}}).Do(req)
			ehttptest.Eq(t, "request", err, nil)

			defer resp.Body.Close()

			ehttptest.Eq(t, "overflow status", resp.StatusCode, http.StatusRequestEntityTooLarge)
			ehttptest.True(t, "net/http MaxBytesReader: Connection: close", resp.Close, "connection stays open")
		})
	}
}

// F6: an interceptor cannot flush. ResponseController must say so.
func TestFlushInsideInterceptorIsNotSupported(t *testing.T) {
	t.Parallel()

	var got error

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		got = http.NewResponseController(res).Flush()
		res.String("x")
	}, middleware.WeakEtag())

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.True(t, "ResponseController.Flush", errors.Is(got, http.ErrNotSupported), "got %v", got)
	ehttptest.Eq(t, "body still sent once", rec.Body.String(), "x")
}
