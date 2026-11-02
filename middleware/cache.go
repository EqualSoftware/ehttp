package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// Cache decides Cache-Control for each response with a status below 400.
// ErrorCache decides the error statuses. They never decide the same status.
// The decision is made when the handler or a middleware commits a status. A
// handler that writes nothing gets 200 with no header. A 101 gets
// none.
//
// The value is "max-age=N, stale-while-revalidate=N, stale-if-error=N", with
// "private, " in front for a request with Authorization (RFC 9111 §3.5). N is
// the whole seconds of the duration, so a duration under 1 second gives
// max-age=0.
// Cache(0) sends "no-store, no-cache, must-revalidate, proxy-revalidate".
// ErrorCache(0) sends only "no-store".
//
// A Cache-Control that the handler or an inner middleware already set wins.
//
// Global and route: when several Cache apply to a request, the one closest to
// the handler wins. A route Cache wins over a global one.
//
// Write the response from the handler goroutine only. A write after the
// handler returns, from another goroutine, is not supported (like
// ResponseWriter).
//
// A Cache outside Timeout does not cover the 503 of Timeout. Use ErrorCache
// for it.
func Cache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	if duration < 0 {
		hook.BootPanic("Cache", "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
			"use zero for no-store, or a positive duration, for example Cache(time.Minute)")
	}

	values := [2]string{ehttp.CacheControl(duration, false), ehttp.CacheControl(duration, true)}

	return cacheControlMiddleware(func(code int, auth bool) string {
		if code >= 400 || code == ehttp.StatusSwitchingProtocols {
			return ""
		}

		if auth {
			return values[1]
		}

		return values[0]
	})
}
