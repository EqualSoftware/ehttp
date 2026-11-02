package middleware

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
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
// A value in any other form panics at boot.
func BodyLimit(l string) ehttp.MiddlewareHandlerFunc {
	limit, ok := parseBodyLimit(l)
	if !ok {
		hook.BootPanic("BodyLimit", "limit "+strconv.Quote(l), "a size is a positive number with an optional unit K, M, G, T or P, and an optional B (author decision)",
			`pass a value like "512", "4K", "4KB" or "1.5M"`)
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

			req.Request.Body = http.MaxBytesReader(rawWriter(res), req.Request.Body, limit)
			next.ServeHTTP(res, req)
		}
	}
}

// parseBodyLimit parses a size like "4K" or "1.5MB". It reports false for
// an empty, signed, spaced, non-positive or too large value.
func parseBodyLimit(l string) (int64, bool) {
	l = strings.ToUpper(l)
	n := 0

	for n < len(l) && (l[n] >= '0' && l[n] <= '9' || l[n] == '.') {
		n++
	}

	v, err := strconv.ParseFloat(l[:n], 64)
	if err != nil || v <= 0 {
		return 0, false
	}

	var mult float64

	switch strings.TrimSuffix(l[n:], "B") {
	case "":
		mult = 1
	case "K":
		mult = kibibyte
	case "M":
		mult = mebibyte
	case "G":
		mult = gibibyte
	case "T":
		mult = tebibyte
	case "P":
		mult = pebibyte
	default:
		return 0, false
	}

	if v*mult >= math.MaxInt64 {
		return 0, false
	}

	limit := int64(v * mult)

	return limit, limit > 0
}

// rawWriter unwraps w down to the real writer. MaxBytesReader never writes. It
// only needs the real writer to ask for "Connection: close" on overflow.
func rawWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}

		w = u.Unwrap()
	}
}
