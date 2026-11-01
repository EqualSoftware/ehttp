package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/EqualSoftware/ehttp"
)

// bytes in IEC
const (
	kibibyte = 1024
	mebibyte = 1024 * kibibyte
	gibibyte = 1024 * mebibyte
	tebibyte = 1024 * gibibyte
	pebibyte = 1024 * tebibyte
)

// Maximum allowed size for a request body, it can be specified
// as `4x` or `4xB`, where x is one of the multiple from K, M, G, T or P.
func BodyLimit(l string) ehttp.MiddlewareHandlerFunc {
	l = strings.ToUpper(l)
	last_n := len(l)

	for i := len(l) - 1; i >= 0; i-- {
		c := l[i]

		if c >= '0' && c <= '9' {
			break
		}

		last_n = i
	}

	v, _ := strconv.ParseFloat(l[:last_n], 64)
	u := l[last_n:]

	var limit int64

	switch u {
	case "KB", "K":
		limit = int64(v * kibibyte)
	case "MB", "M":
		limit = int64(v * mebibyte)
	case "GB", "G":
		limit = int64(v * gibibyte)
	case "TB", "T":
		limit = int64(v * tebibyte)
	case "PB", "P":
		limit = int64(v * pebibyte)
	default:
		limit = int64(v)
	}

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			if req.Request.ContentLength == 0 {
				next.ServeHTTP(res, req)
				return
			}

			if req.Request.ContentLength > limit {
				res.SendStatus(ehttp.StatusRequestEntityTooLarge)
				return
			}

			req.Request.Body = http.MaxBytesReader(res.Writer, req.Request.Body, limit)
			next.ServeHTTP(res, req)
		}
	}
}
