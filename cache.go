package ehttp

import (
	"strconv"
	"strings"
	"time"
)

// SetCacheHeader sets Cache-Control with Set. It is for a handler. A
// middleware that must own the field uses CacheControl with Own.
func SetCacheHeader(r *ResponseWriter, d time.Duration) {
	SetCacheHeaderPrivate(r, d, false)
}

// SetCacheHeaderPrivate is SetCacheHeader with a private flag. If private is
// true and d is not 0, the response gets the `private` directive: a shared
// cache must not store it (RFC 9111 §3.5).
func SetCacheHeaderPrivate(r *ResponseWriter, d time.Duration, private bool) {
	r.Set(HeaderCacheControl, CacheControl(d, private))
}

// CacheControl returns the Cache-Control value of SetCacheHeaderPrivate. It
// has no side effect. A middleware passes it to Own.
func CacheControl(d time.Duration, private bool) string {
	if d == 0 {
		return "no-store, no-cache, must-revalidate, proxy-revalidate"
	}

	max_age := strconv.Itoa(int(d.Seconds()))
	stale_while_revalidate := strconv.Itoa(int(d.Seconds()))
	stale_if_error := strconv.Itoa(int(d.Seconds()))
	cache_control := []string{
		"max-age=" + max_age,
		"stale-while-revalidate=" + stale_while_revalidate,
		"stale-if-error=" + stale_if_error,
	}

	if private {
		cache_control = append([]string{"private"}, cache_control...)
	}

	return strings.Join(cache_control, ", ")
}
