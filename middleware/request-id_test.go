package middleware_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

var bUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRequestId(t *testing.T) {
	t.Parallel()

	t.Run("generates a UUIDv4 when absent", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.RequestId()), fixtures.Req("GET", "/x"))
		id := rec.Header().Get("X-Request-Id")

		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
		ehttptest.True(t, "RFC 9562 §5.4", bUUIDv4.MatchString(id), "generated id %q is not a UUIDv4", id)
	})

	t.Run("each request gets its own id", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(fixtures.OK, middleware.RequestId())
		a := ehttptest.Do(h, fixtures.Req("GET", "/x")).Header().Get("X-Request-Id")
		b := ehttptest.Do(h, fixtures.Req("GET", "/x")).Header().Get("X-Request-Id")

		ehttptest.True(t, "middleware contract", a != "" && a != b, "ids %q and %q", a, b)
	})

	t.Run("propagates a received id", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.RequestId()), fixtures.Req("GET", "/x", "X-Request-Id", "trace-123_abc.4"))

		ehttptest.Eq(t, "middleware contract (propagation)", rec.Header().Get("X-Request-Id"), "trace-123_abc.4")
	})

	t.Run("calls next and keeps its response", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.Status(201, "made"), middleware.RequestId()), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Code, 201)
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), "made")
		ehttptest.True(t, "middleware contract", rec.Header().Get("X-Request-Id") != "", "no id on a 201")
	})

	t.Run("handler running after it can see the id on the response", func(t *testing.T) {
		t.Parallel()

		var seen string
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			seen = res.Header().Get("X-Request-Id")
			res.String("ok")
		}, middleware.RequestId())

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.True(t, "middleware order", seen != "" && seen == rec.Header().Get("X-Request-Id"), "seen %q", seen)
	})
}

// bValidID is the rule for an incoming id: it is kept only when
// it has 1 to 128 characters, all in [A-Za-z0-9._-].
var bValidID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// An id from the client is echoed back, so its length and charset are
// limited. An id that breaks the rule is replaced by a generated one that
// follows the same rule.
func TestRequestIdUntrustedInput(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(fixtures.OK, middleware.RequestId())

	t.Run("a valid 128-character id is kept", func(t *testing.T) {
		t.Parallel()

		id := strings.Repeat("aZ09._-", 19)[:128]
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "X-Request-Id", id))

		ehttptest.Eq(t, "a valid id (1 to 128 characters of [A-Za-z0-9._-]) is kept", rec.Header().Get("X-Request-Id"), id)
	})

	t.Run("replaces an empty id", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "X-Request-Id", ""))
		got := rec.Header().Get("X-Request-Id")

		ehttptest.True(t, "the response id has 1 to 128 characters", bValidID.MatchString(got), "response id %q breaks the rule", got)
	})

	tests := []struct{ name, id string }{
		{"129 characters", strings.Repeat("a", 129)},
		{"1000 characters", strings.Repeat("a", 1000)},
		{"100000 characters", strings.Repeat("a", 100_000)},
		{"space", "a b"},
		{"slash", "a/b"},
		{"colon", "a:b"},
		{"percent", "a%20b"},
		{"markup", "<script>alert(1)</script>"},
		{"carriage return", "a\rb"},
		{"line feed", "a\nb"},
		{"non-ASCII", "id-é世"},
	}

	for _, tt := range tests {
		t.Run("replaces "+tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "X-Request-Id", tt.id))
			got := rec.Header().Get("X-Request-Id")

			ehttptest.True(t, "an invalid id is replaced", got != tt.id, "untrusted id echoed back unchanged (len %d)", len(got))
			ehttptest.True(t, "the new id follows the same rule", bValidID.MatchString(got), "response id %q breaks the rule", got)
		})
	}
}
