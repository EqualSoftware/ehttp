// Package etag reads the entity-tag lists of the If-Match and If-None-Match
// fields (RFC 9110 section 13.1.1 and 13.1.2).
package etag

import (
	"net/http"
	"strings"
)

// ValidRequest reports that If-Match and If-None-Match, in all their field
// lines, are valid. A field that is not there is valid.
func ValidRequest(h http.Header) bool {
	return Valid(h.Values("If-Match")) && Valid(h.Values("If-None-Match"))
}

// Valid reports that each value is "*" alone or a list of entity-tags
// (RFC 9110 section 13.1.1, 13.1.2). A field with no member is valid: the
// list allows zero elements (section 5.6.1.2).
func Valid(values []string) bool {
	_, ok := scan(values, nil)
	return ok
}

// Match reports that the field is valid and has "*" or a tag with the
// opaque value opaque, by weak comparison. An empty list does not match.
func Match(values []string, opaque string) bool {
	match, ok := scan(values, func(tag string) bool { return strings.TrimPrefix(tag, "W/") == opaque })
	return match && ok
}

// MatchStrong reports that the field is valid and has "*" or a tag equal to
// current by strong comparison: neither tag is weak and the opaque values are
// equal character by character (RFC 9110 section 8.8.3.2). It is the
// comparison of If-Match (section 13.1.1). A weak current tag matches only "*".
func MatchStrong(values []string, current string) bool {
	match, ok := scan(values, func(tag string) bool {
		return tag == current && !strings.HasPrefix(tag, "W/")
	})

	return match && ok
}

// scan reads the field lines. match is true when "*" or a tag satisfies same
// (nil never matches a tag). ok is false when a member is not an
// entity-tag, or "*" is not the only member.
func scan(values []string, same func(tag string) bool) (match, ok bool) {
	members, star := 0, false

	for _, list := range values {
		for {
			list = strings.TrimLeft(list, " \t,")

			if list == "" {
				break
			}

			n, tag := scanTag(list)

			if n == 0 {
				return false, false
			}

			members++
			star = star || tag == "*"
			match = match || tag == "*" || same != nil && same(tag)

			list = strings.TrimLeft(list[n:], " \t")

			if list != "" && list[0] != ',' {
				return false, false
			}
		}
	}

	return match, !star || members == 1
}

// scanETag returns the length of the member at the start of s: "*" or an
// entity-tag ([W/] and a quoted string of etagc, RFC 9110 section 8.8.3). It
// returns 0 for anything else.
func scanTag(s string) (n int, tag string) {
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
