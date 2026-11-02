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

const noStore = "no-store"

// errorCacheConfig holds the lifetimes: the default and the per-status ones.
type errorCacheConfig struct {
	duration time.Duration
	byStatus map[int]time.Duration
}

// ErrorCacheOption changes the lifetime of one error status.
type ErrorCacheOption func(*errorCacheConfig)

// ErrorCacheStatus sets the lifetime of one error status (400 to 599). Zero
// turns the header off for this status. Each status can be set one time. It
// panics at boot for 428, 429, 431 and 511: those responses are never stored.
func ErrorCacheStatus(status int, duration time.Duration) ErrorCacheOption {
	return func(cfg *errorCacheConfig) {
		where := "ErrorCacheStatus"
		code := strconv.Itoa(status)

		if status < 400 || status > 599 {
			hook.BootPanic(where, "status "+code, "ErrorCache handles only error statuses, 400 to 599",
				"use a status from 400 to 599, for example ErrorCacheStatus(405, 5*time.Second)")
		}

		if section, ok := neverStored[status]; ok {
			hook.BootPanic(where, "status "+code, "RFC 6585 "+section+`: "Responses with the `+code+` status code MUST NOT be stored by a cache"`,
				"remove ErrorCacheStatus("+code+", ...): ErrorCache always sends no-store for it")
		}

		if duration < 0 {
			hook.BootPanic(where, "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
				"use zero for no header, or a positive duration, for example ErrorCacheStatus(405, 5*time.Second)")
		}

		if _, ok := cfg.byStatus[status]; ok {
			hook.BootPanic(where, "status "+code+" twice", "the second lifetime would replace the first one",
				"set each status one time")
		}

		if cfg.byStatus == nil {
			cfg.byStatus = map[int]time.Duration{}
		}

		cfg.byStatus[status] = duration
	}
}

// neverStored maps each status that a cache must not store to its section
// in RFC 6585.
var neverStored = map[int]string{
	ehttp.StatusPreconditionRequired:          "§3",
	ehttp.StatusTooManyRequests:               "§4",
	ehttp.StatusRequestHeaderFieldsTooLarge:   "§5",
	ehttp.StatusNetworkAuthenticationRequired: "§6",
}

// heuristic reports whether a status is cacheable by default (RFC 9110 §15.1).
func heuristic(code int) bool {
	switch code {
	case ehttp.StatusNotFound, ehttp.StatusMethodNotAllowed, ehttp.StatusGone, ehttp.StatusRequestURITooLong,
		ehttp.StatusNotImplemented:
		return true
	}

	return false
}

// value returns the Cache-Control of an error status; empty means no header.
func (cfg *errorCacheConfig) value(code int, auth bool) string {
	if code < 400 || code > 599 {
		return ""
	}

	if _, never := neverStored[code]; never {
		return noStore
	}

	d, set := cfg.byStatus[code]
	if !set && !heuristic(code) {
		return noStore
	}

	if !set {
		d = cfg.duration
	}

	switch {
	case d > 0:
		private := auth || code == ehttp.StatusUnauthorized || code == ehttp.StatusForbidden || code == ehttp.StatusProxyAuthRequired

		return ehttp.CacheControl(d, private)
	case set:
		return "" // the author turned this status off
	}

	return noStore
}

// ErrorCache sets Cache-Control on each error response (status 400 to 599)
// that has none. A Cache-Control from the handler or an inner middleware wins.
//
// Defaults, by status:
//   - 404, 405, 410, 414 and 501 get the lifetime duration: they are
//     heuristically cacheable (RFC 9110 §15.1). With duration zero they get
//     no-store.
//   - 428, 429, 431 and 511 always get no-store: "MUST NOT be stored by a
//     cache" (RFC 6585 §3 to §6). ErrorCacheStatus panics at boot for them.
//   - Each other error status gets no-store, unless ErrorCacheStatus sets a
//     lifetime for it. A 503 is temporary (RFC 9110 §15.6.4), so keep its
//     lifetime short or zero.
//   - ErrorCacheStatus(code, 0) sends no header for that status.
//
// A lifetime on 401, 403 or 407, or on a response to a request with
// Authorization, is private (RFC 9111 §5.2.2.7, §3.5).
//
// The default is no-store because of cache-poisoned denial of service (CPDoS):
// a bad header can make the origin answer with an error, and a cache that
// stores that error by URL then serves it to every client.
//
// It needs only the status, so it does not buffer the body: the writer reads
// the final status and passes every write through.
func ErrorCache(duration time.Duration, opts ...ErrorCacheOption) ehttp.MiddlewareHandlerFunc {
	cfg := &errorCacheConfig{duration: duration}

	if duration < 0 {
		hook.BootPanic("ErrorCache", "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
			"use zero for no-store, or a positive duration, for example ErrorCache(time.Minute)")
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return cacheControlMiddleware(cfg.value)
}

// cacheControlMiddleware wraps the writer. value gives the Cache-Control for
// the final status, or empty for none.
func cacheControlMiddleware(value func(code int, auth bool) string) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			cc := &cacheControlWriter{value: value, req: req}
			restore := hook.WrapWriter(res, func(w http.ResponseWriter) http.ResponseWriter {
				cc.ResponseWriter = w
				return cc
			})
			defer restore()

			next.ServeHTTP(res, req)
		}
	}
}

// cacheControlWriter adds Cache-Control at the final status, before the header
// goes on the wire. It fills the header only when it is empty.
type cacheControlWriter struct {
	http.ResponseWriter
	value func(code int, auth bool) string
	req   *ehttp.Request
	done  bool
}

func (c *cacheControlWriter) WriteHeader(code int) {
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

func (c *cacheControlWriter) setCacheControl(code int) {
	h := c.Header()
	if len(h[ehttp.HeaderCacheControl]) != 0 {
		return
	}

	if v := c.value(code, c.req.Header(ehttp.HeaderAuthorization) != ""); v != "" {
		h.Set(ehttp.HeaderCacheControl, v)
	}
}

func (c *cacheControlWriter) Write(b []byte) (int, error) {
	c.WriteHeader(ehttp.StatusOK)

	return c.ResponseWriter.Write(b)
}

func (c *cacheControlWriter) Flush() {
	_ = c.FlushError()
}

func (c *cacheControlWriter) FlushError() error {
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// Hijack keeps the behavior of the real writer.
func (c *cacheControlWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(c.ResponseWriter).Hijack()
}

func (c *cacheControlWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}
