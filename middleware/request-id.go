package middleware

import (
	"github.com/EqualSoftware/ehttp"
	"github.com/google/uuid"
)

func RequestId() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			rid := req.Header(ehttp.HeaderXRequestID)

			if !validRequestID(rid) {
				rid = uuid.New().String()
				// The logger of the request (req.Logger) reads the id from here.
				req.Request.Header.Set(ehttp.HeaderXRequestID, rid)
			}

			res.Own(ehttp.HeaderXRequestID, rid)

			next.ServeHTTP(res, req)
		}
	}
}

// validRequestID reports whether an incoming id may be kept: 1 to 128
// characters, all in [A-Za-z0-9._-]. Any other id is untrusted input for
// logs and headers, so a new one replaces it.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}

	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}

		return false
	}

	return true
}
