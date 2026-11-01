package middleware

import (
	"fmt"
	"hash/crc32"
	"math"
	"strconv"
	"strings"

	"github.com/EqualSoftware/ehttp"
)

func WeakEtag() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			method := req.Method()

			if method != ehttp.MethodGet && method != ehttp.MethodHead {
				next.ServeHTTP(res, req)
				return
			}

			skip_body := method == ehttp.MethodHead
			ires, i := ehttp.WithResponseWriter(res)
			next.ServeHTTP(ires, req)

			if skip_body {
				res.Append(ehttp.HeaderContentLength, strconv.FormatInt(i.Size, 10))
			}

			if i.Code != ehttp.StatusOK || i.Size == 0 {
				i.SendResponse(res)
				return
			}

			if i.Size > math.MaxUint32 {
				res.SendStatus(ehttp.StatusRequestEntityTooLarge)
				return
			}

			etag := fmt.Sprintf(`W/"%d-%08x"`, i.Size, crc32.ChecksumIEEE(i.Buffer.Bytes()))
			res.Append(ehttp.HeaderEtag, etag)

			if_none_match := req.Header(ehttp.HeaderIfNoneMatch)
			is_same_content := if_none_match == strings.TrimPrefix(etag, "W/") || if_none_match == etag

			if is_same_content {
				res.SendStatus(ehttp.StatusNotModified)
				return
			}

			i.SendResponse(res)
		}
	}
}
