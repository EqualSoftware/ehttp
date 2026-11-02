package middleware

import (
	"github.com/EqualSoftware/ehttp"
	"github.com/google/uuid"
)

func RequestId() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			rid := req.Header(ehttp.HeaderXRequestID)

			if rid == "" {
				rid = uuid.New().String()
				// The logger of the request (req.Logger) reads the id from here.
				req.Request.Header.Set(ehttp.HeaderXRequestID, rid)
			}

			res.Set(ehttp.HeaderXRequestID, rid)

			next.ServeHTTP(res, req)
		}
	}
}
