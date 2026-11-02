// Package redact masks sensitive values before they reach a log or a message.
//
// A Matcher holds a deny list of name patterns. A name is the name of a header
// field, of a query parameter or of a form field. A value whose name matches is
// replaced with Mask. The name stays visible, so a reader sees which field was
// hidden, for example "?api_key=***&page=2".
//
// Pattern syntax. A match ignores case, and "-" equals "_".
//
//	*x*  the name contains x
//	*x   the name ends with x
//	x*   the name starts with x
//	x    the name is x
//
// The default list follows the default deny lists of the Sentry SDKs, the
// sanitize_field_names setting of Elastic APM, the url.query redaction of the
// OpenTelemetry HTTP semantic conventions and the OWASP Logging Cheat Sheet:
// passwords, secrets, tokens, authentication data, sessions, cookies, CSRF
// tokens, signatures, credentials, names that end with "key", "sig" and
// "AWSAccessKeyId".
//
// A Matcher never changes after it is built, so many goroutines can share it.
// A nil *Matcher works like the default one.
package redact

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Mask replaces a sensitive value.
const Mask = "***"

// defaults is the default deny list.
var defaults = [...]string{
	"*password*", "*passwd*", "*pwd*", "*secret*", "*token*", "*auth*",
	"*session*", "*cookie*", "*csrf*", "*xsrf*", "*signature*", "*credential*",
	"*key", "sig", "AWSAccessKeyId",
}

// Matcher decides if a name is sensitive. Its patterns are lowercase, with "_"
// in place of "-", so a match needs no allocation.
type Matcher struct {
	contains []string
	suffix   []string
	prefix   []string
	exact    []string
}

var defaultMatcher = sync.OnceValue(func() *Matcher {
	m, err := (&Matcher{}).with(defaults[:])
	if err != nil {
		panic("redact: invalid default pattern: " + err.Error())
	}

	return m
})

// Default returns the shared matcher with the default list. Do not change it:
// it has no method that changes it.
func Default() *Matcher { return defaultMatcher() }

// With returns a new matcher that has the patterns of m and the extra ones.
// m does not change. An invalid pattern returns an error that says why.
func (m *Matcher) With(extra ...string) (*Matcher, error) {
	if m == nil {
		m = Default()
	}

	return m.with(extra)
}

func (m *Matcher) with(extra []string) (*Matcher, error) {
	n := &Matcher{
		contains: slices.Clone(m.contains),
		suffix:   slices.Clone(m.suffix),
		prefix:   slices.Clone(m.prefix),
		exact:    slices.Clone(m.exact),
	}

	for _, p := range extra {
		if err := n.add(p); err != nil {
			return nil, err
		}
	}

	return n, nil
}

func (m *Matcher) add(p string) error {
	core := p
	front := strings.HasPrefix(core, "*")
	core = strings.TrimPrefix(core, "*")
	back := strings.HasSuffix(core, "*")
	core = strings.TrimSuffix(core, "*")

	switch {
	case p == "":
		return errors.New("it is empty")
	case core == "":
		return errors.New("it has no name, only \"*\"")
	case strings.Contains(core, "*"):
		return errors.New("a \"*\" can only be the first or the last byte, one time")
	}

	for i := 0; i < len(core); i++ {
		if !tchar(core[i]) {
			return errors.New("it has a byte that is not valid in a header or query name")
		}
	}

	core = normalize(core)

	switch {
	case front && back:
		m.contains = append(m.contains, core)
	case front:
		m.suffix = append(m.suffix, core)
	case back:
		m.prefix = append(m.prefix, core)
	default:
		m.exact = append(m.exact, core)
	}

	return nil
}

// tchar reports that c is a token byte (RFC 9110 section 5.6.2), without "*".
func tchar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}

	return c != '*' && strings.IndexByte("!#$%&'+-.^_`|~", c) >= 0
}

func normalize(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = fold(b[i])
	}

	return string(b)
}

// fold lowercases c and maps "-" to "_".
func fold(c byte) byte {
	switch {
	case c >= 'A' && c <= 'Z':
		return c + 'a' - 'A'
	case c == '-':
		return '_'
	}

	return c
}

// equalAt reports that name[i:] starts with p (p is normalized).
func equalAt(name string, i int, p string) bool {
	if len(name)-i < len(p) {
		return false
	}

	for j := 0; j < len(p); j++ {
		if fold(name[i+j]) != p[j] {
			return false
		}
	}

	return true
}

// Sensitive reports that the value of the field name must be masked. It does
// not allocate.
func (m *Matcher) Sensitive(name string) bool {
	if m == nil {
		m = Default()
	}

	for _, p := range m.exact {
		if len(name) == len(p) && equalAt(name, 0, p) {
			return true
		}
	}

	for _, p := range m.prefix {
		if equalAt(name, 0, p) {
			return true
		}
	}

	for _, p := range m.suffix {
		if len(name) >= len(p) && equalAt(name, len(name)-len(p), p) {
			return true
		}
	}

	for _, p := range m.contains {
		for i := 0; i+len(p) <= len(name); i++ {
			if equalAt(name, i, p) {
				return true
			}
		}
	}

	return false
}

// Value returns value, or Mask if the field name is sensitive. It does not
// allocate.
func (m *Matcher) Value(name, value string) string {
	if m.Sensitive(name) {
		return Mask
	}

	return value
}

// Header returns a copy of h with the values of sensitive fields masked: each
// line of a field becomes Mask. h does not change. A nil h returns nil.
func (m *Matcher) Header(h http.Header) http.Header {
	if h == nil {
		return nil
	}

	c := make(http.Header, len(h))

	for name, values := range h {
		out := make([]string, len(values))

		for i, v := range values {
			out[i] = m.Value(name, v)
		}

		c[name] = out
	}

	return c
}

// URL returns u as a string that is safe to log. Userinfo becomes "***:***@",
// or "***@" if there is no password. The value of each sensitive query
// parameter, also of a repeated key, becomes Mask. The path stays and the
// fragment goes. The path is encoded again by url.URL.String (a space becomes
// "%20"). "&" and ";" both separate query parameters (RFC 3986 section 3.4),
// and a key that is empty or does not decode is masked. u does not change. A nil u returns "".
func (m *Matcher) URL(u *url.URL) string {
	if u == nil {
		return ""
	}

	if u.Opaque != "" {
		// The opaque part can hold userinfo ("http:user:pass@host/x").
		return u.Scheme + ":" + Mask + queryPart(m.Query(u.RawQuery), u.ForceQuery)
	}

	c := *u
	c.Fragment, c.RawFragment = "", ""

	c.User = nil // String would escape the mask

	c.RawQuery = m.Query(u.RawQuery)
	str := c.String()

	i := strings.Index(str, "//")
	if u.User == nil || i < 0 {
		return str
	}

	info := Mask
	if _, ok := u.User.Password(); ok {
		info = Mask + ":" + Mask
	}

	return str[:i+2] + info + "@" + str[i+2:]
}

// Query masks the values of the sensitive parameters of a raw query. The text
// stays as it is, apart from the masked values. It allocates only if a
// parameter is sensitive.
func (m *Matcher) Query(raw string) string {
	var out strings.Builder

	done := 0 // raw[:done] is already in out

	for pos := 0; pos < len(raw); {
		end := len(raw)
		if i := strings.IndexAny(raw[pos:], "&;"); i >= 0 {
			end = pos + i
		}

		eq := strings.IndexByte(raw[pos:end], '=')
		if eq >= 0 && m.sensitiveKey(raw[pos:pos+eq]) {
			out.WriteString(raw[done : pos+eq+1])
			out.WriteString(Mask)

			done = end
		}

		pos = end + 1
	}

	if done == 0 {
		return raw
	}

	out.WriteString(raw[done:])

	return out.String()
}

func queryPart(q string, force bool) string {
	if q == "" && !force {
		return ""
	}

	return "?" + q
}

// sensitiveKey decodes a query key only if it has an escape.
func (m *Matcher) sensitiveKey(key string) bool {
	if strings.ContainsAny(key, "%+") {
		k, err := url.QueryUnescape(key)
		if err != nil {
			return true // a key that does not decode is masked
		}

		key = k
	}

	if key == "" {
		return true
	}

	return m.Sensitive(key)
}
