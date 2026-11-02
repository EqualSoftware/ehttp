package middleware

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// errorCacheConfig holds the lifetimes: the default and the per-status ones.
type errorCacheConfig struct {
	duration time.Duration
	byStatus map[int]time.Duration
}

// ErrorCacheOption changes the lifetime of one error status.
type ErrorCacheOption func(*errorCacheConfig)

// ErrorCacheStatus sets the lifetime of one error status (400 to 599). Zero
// turns the header off for this status. Each status can be set one time.
func ErrorCacheStatus(status int, duration time.Duration) ErrorCacheOption {
	return func(cfg *errorCacheConfig) {
		where := "ErrorCacheStatus"

		if status < 400 || status > 599 {
			hook.BootPanic(where, "status "+strconv.Itoa(status), "ErrorCache handles only error statuses, 400 to 599",
				"use a status from 400 to 599, for example ErrorCacheStatus(405, 5*time.Second)")
		}

		if duration < 0 {
			hook.BootPanic(where, "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
				"use zero for no header, or a positive duration, for example ErrorCacheStatus(405, 5*time.Second)")
		}

		if _, ok := cfg.byStatus[status]; ok {
			hook.BootPanic(where, "status "+strconv.Itoa(status)+" twice", "the second lifetime would replace the first one",
				"set each status one time")
		}

		if cfg.byStatus == nil {
			cfg.byStatus = map[int]time.Duration{}
		}

		cfg.byStatus[status] = duration
	}
}

// lifetime returns the lifetime of a status.
func (cfg *errorCacheConfig) lifetime(code int) time.Duration {
	if d, ok := cfg.byStatus[code]; ok {
		return d
	}

	return cfg.duration
}

// ErrorCache sets Cache-Control on each error response (status 400 to 599)
// that has none. The default lifetime is duration. ErrorCacheStatus sets the
// lifetime of one status. Zero turns the header off. A Cache-Control from the
// handler or an inner middleware wins. A response with status 401, 403 or 407,
// or to a request with Authorization, is private (RFC 9111 §5.2.2.7). A 503
// is temporary and not heuristically cacheable (RFC 9110 §15.6.4): give it a
// short or zero lifetime, for example ErrorCacheStatus(503, 0). It needs
// only the status, so it does not buffer the body: the writer reads the final
// status and passes every write through.
func ErrorCache(duration time.Duration, opts ...ErrorCacheOption) ehttp.MiddlewareHandlerFunc {
	cfg := &errorCacheConfig{duration: duration}

	if duration < 0 {
		hook.BootPanic("ErrorCache", "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
			"use zero for no header, or a positive duration, for example ErrorCache(time.Minute)")
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			cc := &errorCacheWriter{cfg: cfg, req: req}
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
	cfg  *errorCacheConfig
	req  *ehttp.Request
	done bool
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

	c.setCacheControl(code)
	c.ResponseWriter.WriteHeader(code)
}

// setCacheControl sets the header on an error status, only when it is empty.
func (c *errorCacheWriter) setCacheControl(code int) {
	if code < 400 || code > 599 {
		return
	}

	d := c.cfg.lifetime(code)
	if d == 0 || c.Header().Get(ehttp.HeaderCacheControl) != "" {
		return
	}

	private := alwaysPrivate(code) || c.req.Header(ehttp.HeaderAuthorization) != ""
	c.Header().Set(ehttp.HeaderCacheControl, ehttp.CacheControl(d, private))
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

// alwaysPrivate reports whether the response depends on the credentials of
// the caller: 401, 403 and 407.
func alwaysPrivate(code int) bool {
	return code == ehttp.StatusUnauthorized || code == ehttp.StatusForbidden || code == ehttp.StatusProxyAuthRequired
}
