package middleware

import (
	"bufio"
	"io"
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
	// Built once at boot: the header values, [0] public and [1] private.
	def    [2]string
	values map[int][2]string
}

// build computes the header values, so a request only reads them.
func (cfg *errorCacheConfig) build() {
	cfg.def = lifetimeValues(cfg.duration, true)
	cfg.values = make(map[int][2]string, len(cfg.byStatus))

	for code, d := range cfg.byStatus {
		cfg.values[code] = lifetimeValues(d, false)
	}
}

// lifetimeValues returns the public and private values of a lifetime. Zero
// is no-store for a default and no header for a status that the author set.
func lifetimeValues(d time.Duration, isDefault bool) [2]string {
	if d == 0 {
		if isDefault {
			return [2]string{noStore, noStore}
		}

		return [2]string{}
	}

	return [2]string{ehttp.CacheControl(d, false), ehttp.CacheControl(d, true)}
}

// ErrorCacheOption changes the lifetime of one error status.
type ErrorCacheOption func(*errorCacheConfig)

// ErrorCacheStatus sets the lifetime of one error status (400 to 599) for the
// ErrorCache that gets it.
//
// A lifetime above zero sets the header like the ErrorCache default:
// "max-age=N, stale-while-revalidate=N, stale-if-error=N", with "private, " in
// front for 401, 403, 407 and for a request with Authorization.
//
// Zero sends no header from this ErrorCache. A Cache-Control from an ErrorCache
// further out can still fill the header, so the outermost off switch is final.
//
// Each status can be set one time. The call panics at boot for a status
// outside 400 to 599, a negative lifetime, and 428, 429, 431 and 511: a cache
// must not store those (RFC 6585).
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

	v, set := cfg.values[code]
	if !set && !heuristic(code) {
		return noStore
	}

	if !set {
		v = cfg.def
	}

	if auth || code == ehttp.StatusUnauthorized || code == ehttp.StatusForbidden || code == ehttp.StatusProxyAuthRequired {
		return v[1]
	}

	return v[0]
}

// ErrorCache decides Cache-Control for each error response (status 400 to
// 599). Cache decides the statuses below 400. They never decide the same status.
//
// Default for each status:
//   - 404, 405, 410, 414, 501: the lifetime duration, because they are
//     heuristically cacheable (RFC 9110 §15.1). With duration zero: no-store.
//   - 428, 429, 431, 511: always no-store. "MUST NOT be stored by a cache"
//     (RFC 6585 §3 to §6). ErrorCacheStatus panics at boot for them.
//   - Each other error status: no-store, unless ErrorCacheStatus sets a
//     lifetime for it. A 503 is temporary (RFC 9110 §15.6.4): keep its
//     lifetime short or zero.
//
// The default is no-store because of cache-poisoned denial of service (CPDoS):
// a bad request header can make the origin send an error, and a cache that
// stores it by URL serves it to every client.
//
// A lifetime has the value "max-age=N, stale-while-revalidate=N,
// stale-if-error=N". It has "private, " in front for 401, 403 and 407, and for
// a request with Authorization (RFC 9111 §5.2.2.7, §3.5). No-store is the
// plain value "no-store". ErrorCache(0) sends it for the default group.
//
// A Cache-Control that the handler or an inner middleware already set wins.
//
// Global and route: when several ErrorCache apply to a request, the one
// closest to the handler decides. If it sends no header (an off switch), the
// next one out can decide.
//
// Order: put ErrorCache outside WeakEtag to decide the 412 of WeakEtag. Inside,
// the 412 has no Cache-Control, which is safe. A Timeout sets no Cache-Control
// on its 503: put ErrorCache outside Timeout to get no-store (or the lifetime
// of ErrorCacheStatus(503, ...)) on it.
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

	cfg.build()

	return cacheControlMiddleware(cfg.value)
}

// cachePolicy gives the Cache-Control for the final status, or empty for none.
type cachePolicy func(code int, auth bool) string

// cacheControlMiddleware registers a policy for the request. The first policy
// of a request installs the writer. A later policy (a Cache or ErrorCache
// further in) only joins it: no new writer, closure or hook. The policy
// closest to the handler that gives a value wins.
func cacheControlMiddleware(policy cachePolicy) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			if cc, ok := res.Unwrap().(*cacheControlWriter); ok {
				cc.push(policy)
				defer cc.pop()

				next.ServeHTTP(res, req)

				return
			}

			cc := &cacheControlWriter{req: req}
			cc.policies = cc.buf[:0]
			cc.push(policy)

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
	req      *ehttp.Request
	policies []cachePolicy // outermost first
	buf      [2]cachePolicy
	done     bool
}

func (c *cacheControlWriter) push(p cachePolicy) { c.policies = append(c.policies, p) }

func (c *cacheControlWriter) pop() { c.policies = c.policies[:len(c.policies)-1] }

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

	auth := c.req.Header(ehttp.HeaderAuthorization) != ""

	for i := len(c.policies) - 1; i >= 0; i-- {
		if v := c.policies[i](code, auth); v != "" {
			h.Set(ehttp.HeaderCacheControl, v)
			return
		}
	}
}

func (c *cacheControlWriter) Write(b []byte) (int, error) {
	c.WriteHeader(ehttp.StatusOK)

	return c.ResponseWriter.Write(b)
}

// ReadFrom keeps the sendfile path of the real writer.
func (c *cacheControlWriter) ReadFrom(r io.Reader) (int64, error) {
	c.WriteHeader(ehttp.StatusOK)

	if rf, ok := c.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	return io.Copy(struct{ io.Writer }{c.ResponseWriter}, r)
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
