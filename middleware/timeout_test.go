package middleware_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

// The real lifecycle (late writes, panics in the handler goroutine, headers
// map races) needs a live server and belongs to the lifecycle suite.

func TestTimeoutFastHandler(t *testing.T) {
	t.Parallel()

	rec := ehttptest.Do(fixtures.Handler(fixtures.Status(201, "done"), middleware.Timeout(time.Second)), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "middleware contract", rec.Code, 201)
	ehttptest.Eq(t, "middleware contract", rec.Body.String(), "done")
}

func TestTimeoutContextDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		got := make(chan time.Duration, 1)

		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			dl, ok := req.Context().Deadline()
			if !ok {
				got <- -1
			} else {
				got <- dl.Sub(start)
			}

			res.String("ok")
		}, middleware.Timeout(5*time.Second))

		ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "context.WithTimeout contract", <-got, 5*time.Second)
	})
}

func TestTimeoutSlowHandler(t *testing.T) {
	t.Parallel()

	slow := func(seen chan<- error) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			<-req.Context().Done()
			seen <- req.Context().Err()
		}
	}

	t.Run("handler context is cancelled and a timeout status is sent", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			seen := make(chan error, 1)
			start := time.Now()

			rec := ehttptest.Do(fixtures.Handler(slow(seen), middleware.Timeout(time.Second)), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "fake clock: answered at the deadline", time.Since(start), time.Second)
			ehttptest.True(t, "RFC 9110 §15.5.9/§15.6.4", rec.Code == 408 || rec.Code == 503 || rec.Code == 504, "status = %d", rec.Code)

			synctest.Wait()
			ehttptest.Eq(t, "context contract", (<-seen).Error(), "context deadline exceeded")
		})
	})

	t.Run("status is 503", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			ehttptest.KnownBug(t, "S5", func(t testing.TB) {
				seen := make(chan error, 1)
				rec := ehttptest.Do(fixtures.Handler(slow(seen), middleware.Timeout(time.Second)), fixtures.Req("GET", "/x"))

				ehttptest.Eq(t, "S5 / RFC 9110 §15.6.4 (408 is for slow request reads)", rec.Code, 503)
			})
		})
	})
}

// S5: Request.WithContext mutates the shared *Request, so an outer
// middleware sees an already-cancelled context after Timeout returns.
func TestTimeoutRestoresRequestContext(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "S5", func(t testing.TB) {
		var after error
		outer := func(next ehttp.Handler) ehttp.HandlerFunc {
			return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
				next.ServeHTTP(res, req)
				after = req.Context().Err()
			}
		}

		ehttptest.Do(fixtures.Handler(fixtures.OK, outer, middleware.Timeout(time.Second)), fixtures.Req("GET", "/x"))

		ehttptest.True(t, "S5", after == nil, "outer middleware sees ctx error %v after a successful request", after)
	})
}
