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

// C11: an id from the client is echoed back, so it must be bounded and
// limited to a safe charset. A rejected id is replaced by a generated one.
func TestRequestIdUntrustedInput(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, id string }{
		{"1000 characters", strings.Repeat("a", 1000)},
		{"100000 characters", strings.Repeat("a", 100_000)},
		{"markup", "<script>alert(1)</script>"},
		{"spaces and punctuation", "a b;c=d"},
		{"non-ASCII", "id-é世"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "C11", func(t testing.TB) {
				rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.RequestId()), fixtures.Req("GET", "/x", "X-Request-Id", tt.id))
				got := rec.Header().Get("X-Request-Id")

				ehttptest.True(t, "C11", got != tt.id, "untrusted id echoed back unchanged (len %d)", len(got))
			})
		})
	}
}
