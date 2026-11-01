package ehttp

import (
	"strconv"
	"strings"
	"time"
)

func SetCacheHeader(r *ResponseWriter, d time.Duration) {
	SetCacheHeaderPrivate(r, d, false)
}

// SetCacheHeaderPrivate is SetCacheHeader with a private flag. If private is
// true and d is not 0, the response gets the `private` directive: a shared
// cache must not store it (RFC 9111 §3.5).
func SetCacheHeaderPrivate(r *ResponseWriter, d time.Duration, private bool) {
	if d == 0 {
		r.Append(HeaderCacheControl, "no-store, no-cache, must-revalidate, proxy-revalidate")

		return
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

	r.Append(HeaderCacheControl, strings.Join(cache_control, ", "))
}
