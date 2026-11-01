package ehttp

import (
	"strconv"
	"strings"
	"time"
)

func SetCacheHeader(r *ResponseWriter, d time.Duration) {
	if d == 0 {
		r.Append(HeaderCacheControl, "no-store, no-cache, must-revalidate, proxy-revalidate")

		return
	}

	max_age := strconv.Itoa(int(d.Seconds()))
	stale_while_revalidate := strconv.Itoa(int(d.Seconds()))
	stale_if_error := strconv.Itoa(int(d.Seconds()))
	cache_control := []string{
		"public",
		"max-age=" + max_age,
		"stale-while-revalidate=" + stale_while_revalidate,
		"stale-if-error=" + stale_if_error,
	}

	r.Append(HeaderCacheControl, strings.Join(cache_control, ", "))
}
