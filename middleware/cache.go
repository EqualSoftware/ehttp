package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// Cache decides Cache-Control for each response with a status below 400.
// ErrorCache decides the error statuses. They never decide the same status.
//
// The value is "max-age=N, stale-while-revalidate=N, stale-if-error=N", with
// "private, " in front for a request with Authorization (RFC 9111 §3.5).
// Cache(0) sends "no-store, no-cache, must-revalidate, proxy-revalidate".
// ErrorCache(0) sends only "no-store".
//
// A Cache-Control that the handler or an inner middleware already set wins.
//
// Global and route: when several Cache apply to a request, the one closest to
// the handler wins. A route Cache wins over a global one.
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
