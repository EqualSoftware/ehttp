package middleware

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// ErrorCache sets Cache-Control on a cacheable error response. It needs only
// the status, so it does not buffer the body: the writer reads the final
// status and passes every write through.
func ErrorCache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			cc := &errorCacheWriter{duration: duration, req: req}
			restore := hook.WrapWriter(res, func(w http.ResponseWriter) http.ResponseWriter {
				cc.ResponseWriter = w
				return cc
			})
			defer restore()

			next.ServeHTTP(res, req)
		}
	}
}

// errorCacheWriter adds Cache-Control at the final status, before the header
// goes on the wire.
type errorCacheWriter struct {
	http.ResponseWriter
	duration time.Duration
	req      *ehttp.Request
	done     bool
}

func (c *errorCacheWriter) WriteHeader(code int) {
	if c.done {
		return
	}

	if code >= 100 && code <= 199 && code != ehttp.StatusSwitchingProtocols {
		c.ResponseWriter.WriteHeader(code) // interim: not the final status
		return
	}

	c.done = true

	if cacheableError(code) {
		private := c.req.Header(ehttp.HeaderAuthorization) != ""
		c.Header().Set(ehttp.HeaderCacheControl, ehttp.CacheControl(c.duration, private))
	}

	c.ResponseWriter.WriteHeader(code)
}

func (c *errorCacheWriter) Write(b []byte) (int, error) {
	c.WriteHeader(ehttp.StatusOK)

	return c.ResponseWriter.Write(b)
}

func (c *errorCacheWriter) Flush() {
	_ = c.FlushError()
}

func (c *errorCacheWriter) FlushError() error {
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// Hijack keeps the behavior of the real writer.
func (c *errorCacheWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(c.ResponseWriter).Hijack()
}

func (c *errorCacheWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// cacheableError reports whether an error status may be cached by URL.
// The statuses that depend on the client and 503 (temporary) are not cached.
func cacheableError(code int) bool {
	switch code {
	case ehttp.StatusUnauthorized, ehttp.StatusForbidden, ehttp.StatusProxyAuthRequired, ehttp.StatusTooManyRequests,
		ehttp.StatusServiceUnavailable:
		return false
	}

	return code >= 400
}
