package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
)

func ErrorCache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			ires, i := ehttp.WithResponseWriter(res)
			next.ServeHTTP(ires, req)

			if cacheableError(i.Code) {
				setCacheControl(res, req, duration)
			}

			i.SendResponse(res)
		}
	}
}

// cacheableError reports whether an error status may be cached by URL.
// The statuses that depend on the client (S10c) and 503 (temporary) are not cached.
func cacheableError(code int) bool {
	switch code {
	case ehttp.StatusUnauthorized, ehttp.StatusForbidden, ehttp.StatusProxyAuthRequired, ehttp.StatusTooManyRequests,
		ehttp.StatusServiceUnavailable:
		return false
	}

	return code >= 400
}
