package middleware

import (
	"net/http"
	"time"

	"github.com/EqualSoftware/ehttp"
)

func LastModified(last_modified time.Time) ehttp.MiddlewareHandlerFunc {
	last_modified = last_modified.UTC().Truncate(time.Second)

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			method := req.Method()

			if method != ehttp.MethodGet && method != ehttp.MethodHead {
				next.ServeHTTP(res, req)
				return
			}

			res.Append(ehttp.HeaderLastModified, last_modified.Format(http.TimeFormat))

			if_modified_since := req.Header(ehttp.HeaderIfModifiedSince)

			if if_modified_since == "" {
				next.ServeHTTP(res, req)
				return
			}

			if since, err := time.Parse(http.TimeFormat, if_modified_since); err == nil && !last_modified.After(since) {
				res.SendStatus(ehttp.StatusNotModified)
				return
			}

			next.ServeHTTP(res, req)
		}
	}
}
