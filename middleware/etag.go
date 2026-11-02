package middleware

import (
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"

	"github.com/EqualSoftware/ehttp"
)

// WeakEtag adds a weak ETag over the body of a 200 response to GET and HEAD, and
// answers 304 when If-None-Match matches it (weak comparison, RFC 9110
// section 13.1.2). The ETag is over the whole body, so the middleware buffers
// it. An If-Match or If-None-Match with an invalid syntax gets 400.
func WeakEtag() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			method := req.Method()

			if method != ehttp.MethodGet && method != ehttp.MethodHead {
				next.ServeHTTP(res, req)
				return
			}

			header := req.Request.Header
			inm := header.Values(ehttp.HeaderIfNoneMatch)

			if !validETagList(header.Values("If-Match")) || !validETagList(inm) {
				res.SendStatus(ehttp.StatusBadRequest)
				return
			}

			skip_body := method == ehttp.MethodHead
			ires, i := ehttp.WithResponseWriter(res)
			next.ServeHTTP(ires, req)

			if skip_body {
				res.Set(ehttp.HeaderContentLength, strconv.FormatInt(i.Size, 10))
			}

			if i.Code != ehttp.StatusOK || i.Size == 0 {
				i.SendResponse(res)
				return
			}

			etag := fmt.Sprintf(`W/"%d-%08x"`, i.Size, crc32.ChecksumIEEE(i.Buffer.Bytes()))
			res.Set(ehttp.HeaderEtag, etag)

			if matchETagList(inm, strings.TrimPrefix(etag, "W/")) {
				res.SendStatus(ehttp.StatusNotModified)
				return
			}

			i.SendResponse(res)
		}
	}
}

// validETagList reports that each value is "*" alone or a list of entity-tags
// (RFC 9110 section 13.1.1, 13.1.2). A field without a member is not valid.
func validETagList(values []string) bool {
	_, ok := scanETagList(values, "")
	return ok || len(values) == 0
}

// matchETagList reports that the field is valid and has "*" or a tag with the
// opaque value opaque, by weak comparison.
func matchETagList(values []string, opaque string) bool {
	match, ok := scanETagList(values, opaque)
	return match && ok
}

// scanETagList reads the field lines. ok is false when a member is not an
// entity-tag, "*" is not the only member, or the field has no member.
func scanETagList(values []string, opaque string) (match, ok bool) {
	members, star := 0, false

	for _, list := range values {
		for {
			list = strings.TrimLeft(list, " \t,")

			if list == "" {
				break
			}

			n, tag := scanETag(list)

			if n == 0 {
				return false, false
			}

			members++
			star = star || tag == "*"
			match = match || tag == "*" || strings.TrimPrefix(tag, "W/") == opaque

			list = strings.TrimLeft(list[n:], " \t")

			if list != "" && list[0] != ',' {
				return false, false
			}
		}
	}

	return match, members > 0 && (!star || members == 1)
}

// scanETag returns the length of the member at the start of s: "*" or an
// entity-tag ([W/] and a quoted string of etagc, RFC 9110 section 8.8.3). It
// returns 0 for anything else.
func scanETag(s string) (n int, tag string) {
	if s[0] == '*' {
		return 1, "*"
	}

	i := 0

	if strings.HasPrefix(s, "W/") {
		i = 2
	}

	if i >= len(s) || s[i] != '"' {
		return 0, ""
	}

	for i++; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return i + 1, s[:i+1]
		case c < 0x21 || c == 0x7f:
			return 0, ""
		}
	}

	return 0, ""
}
