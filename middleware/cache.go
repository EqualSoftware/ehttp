package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// Cache sets Cache-Control on each response with a status below 400: errors
// belong to ErrorCache. A Cache-Control from the handler or an inner middleware
// wins. A response to a request with Authorization is private (RFC 9111 §3.5).
// Zero sends "no-store, no-cache, must-revalidate, proxy-revalidate" (ErrorCache
// sends only "no-store").
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
