package ehttp_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker/v2"
)

var errBoom = errors.New("boom")

func bBreaker() *ehttp.CircuitBreaker[int] {
	return ehttp.NewCircuitBreaker[int](ehttp.BreakerOptions{Name: "b", Timeout: 30 * time.Second})
}

func bFail(cb *ehttp.CircuitBreaker[int]) error {
	_, err := cb.Execute(func() (int, error) { return 0, errBoom })

	return err
}

func bSucceed(cb *ehttp.CircuitBreaker[int]) error {
	_, err := cb.Execute(func() (int, error) { return 1, nil })

	return err
}

func TestBreakerPassThrough(t *testing.T) {
	t.Parallel()

	cb := bBreaker()

	v, err := cb.Execute(func() (int, error) { return 42, nil })

	ehttptest.True(t, "gobreaker contract", err == nil, "err = %v", err)
	ehttptest.Eq(t, "gobreaker contract", v, 42)
	ehttptest.Eq(t, "gobreaker contract", cb.State().String(), "closed")

	err = bFail(cb)
	ehttptest.True(t, "gobreaker contract (callee error is returned as is)", errors.Is(err, errBoom), "err = %v", err)
}

// The breaker must open on a failure *rate* with a minimum number of
// samples. ReadyToTrip divides ConsecutiveFailures by ConsecutiveSuccesses,
// which is +Inf after any failure that follows zero consecutive successes.
func TestBreakerNeedsEvidenceToOpen(t *testing.T) {
	t.Parallel()

	t.Run("a single failure does not open it", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		_ = bFail(cb)

		ehttptest.Eq(t, "a failure rate with a minimum of samples trips the breaker", cb.State().String(), "closed")
		ehttptest.True(t, "a single failure does not trip the breaker", bSucceed(cb) == nil, "next call was rejected after one failure")
	})

	t.Run("one failure after many successes does not open it", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 99 {
			_ = bSucceed(cb)
		}

		_ = bFail(cb) // 1% failure rate

		ehttptest.Eq(t, "a failure rate with a minimum of samples trips the breaker", cb.State().String(), "closed")
	})

	t.Run("sustained failure opens it and rejects calls", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 100 {
			_ = bFail(cb)
		}

		called := false
		_, err := cb.Execute(func() (int, error) { called = true; return 0, nil })

		ehttptest.Eq(t, "a high failure rate trips the breaker", cb.State().String(), "open")
		ehttptest.True(t, "gobreaker contract", err != nil && !called, "open breaker ran the callee (err=%v)", err)
	})
}

func TestBreakerRecovery(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cb := bBreaker()
		for range 100 {
			_ = bFail(cb)
		}

		ehttptest.Eq(t, "a high failure rate trips the breaker", cb.State().String(), "open")

		time.Sleep(29 * time.Second)
		ehttptest.Eq(t, "gobreaker contract (Timeout not reached)", cb.State().String(), "open")

		time.Sleep(2 * time.Second)
		ehttptest.Eq(t, "gobreaker contract (Timeout elapsed)", cb.State().String(), "half-open")

		ehttptest.True(t, "gobreaker contract (probe passes)", bSucceed(cb) == nil, "probe was rejected")
		ehttptest.Eq(t, "gobreaker contract (probe closes it)", cb.State().String(), "closed")
	})

	synctest.Test(t, func(t *testing.T) {
		cb := bBreaker()
		for range 100 {
			_ = bFail(cb)
		}

		time.Sleep(31 * time.Second)
		_ = bFail(cb) // probe fails

		ehttptest.Eq(t, "gobreaker contract (failed probe reopens)", cb.State().String(), "open")
	})
}

func TestBreakerPrometheusGauge(t *testing.T) {
	t.Parallel()

	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "breaker_state"}, []string{"name"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(gauge)

	value := func() float64 {
		fams, err := reg.Gather()
		if err != nil || len(fams) != 1 || len(fams[0].GetMetric()) != 1 {
			t.Fatalf("gather: %v %v", fams, err)
		}

		return fams[0].GetMetric()[0].GetGauge().GetValue()
	}

	synctest.Test(t, func(t *testing.T) {
		cb := ehttp.NewCircuitBreaker[int](ehttp.BreakerOptions{Name: "up", Timeout: 10 * time.Second, PrometheusGauge: gauge})

		ehttptest.Eq(t, "breaker.go state mapping (closed=0)", value(), 0.0)

		for range 100 {
			_ = bFail(cb)
		}

		ehttptest.Eq(t, "breaker.go state mapping (open=2)", value(), 2.0)

		time.Sleep(11 * time.Second)
		cb.State() // observing the state moves open to half-open
		ehttptest.Eq(t, "breaker.go state mapping (half-open=1)", value(), 1.0)

		_ = bSucceed(cb)
		ehttptest.Eq(t, "breaker.go state mapping (closed=0)", value(), 0.0)
	})
}

// Calls from many goroutines must not race, and the counts must stay right:
// a failure rate above the threshold with enough samples opens the breaker.
func TestBreakerConcurrentCalls(t *testing.T) {
	t.Parallel()

	cb := bBreaker()

	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 20 {
				_ = bFail(cb)
				_ = cb.State()
			}
		}()
	}

	wg.Wait()

	ehttptest.Eq(t, "a high failure rate trips the breaker", cb.State().String(), "open")
}

// The breaker needs 20 requests in the interval and a 50% failure rate.
func TestBreakerBoundary(t *testing.T) {
	t.Parallel()

	t.Run("19 failures stay closed", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 19 {
			_ = bFail(cb)
		}

		ehttptest.Eq(t, "fewer than 20 samples do not trip the breaker", cb.State().String(), "closed")
	})

	t.Run("20 failures open it", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 20 {
			_ = bFail(cb)
		}

		ehttptest.Eq(t, "20 samples with 100% failure trip the breaker", cb.State().String(), "open")
	})

	t.Run("10 successes then 10 failures open it", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 10 {
			_ = bSucceed(cb)
		}

		for range 10 {
			_ = bFail(cb)
		}

		ehttptest.Eq(t, "20 samples with a 50% failure rate trip the breaker", cb.State().String(), "open")
	})

	t.Run("10 successes then 9 failures stay closed", func(t *testing.T) {
		t.Parallel()

		cb := bBreaker()
		for range 10 {
			_ = bSucceed(cb)
		}

		for range 9 {
			_ = bFail(cb)
		}

		_ = bSucceed(cb) // 20 samples, 45% failed

		ehttptest.Eq(t, "a rate below 50% does not trip the breaker", cb.State().String(), "closed")
	})
}

// With no Interval set, the counts of a closed breaker reset after 10s.
func TestBreakerDefaultIntervalResets(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cb := bBreaker()
		for range 19 {
			_ = bFail(cb)
		}

		time.Sleep(11 * time.Second)
		_ = bFail(cb) // the old 19 failures are gone: 1 sample

		ehttptest.Eq(t, "the default interval resets the counts", cb.State().String(), "closed")
	})
}

// The window rolls: the counts leave it bucket by bucket, not all at once.
func TestBreakerRollingWindow(t *testing.T) {
	t.Parallel()

	t.Run("failures inside one window open it", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			cb := bBreaker()
			for range 10 {
				_ = bFail(cb)
			}

			time.Sleep(5 * time.Second)

			for range 10 {
				_ = bFail(cb)
			}

			ehttptest.Eq(t, "20 failures in 5 s are in the 10 s window", cb.State().String(), "open")
		})
	})

	t.Run("old failures roll off", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			cb := bBreaker()
			for range 10 {
				_ = bFail(cb)
			}

			time.Sleep(11 * time.Second)

			for range 10 {
				_ = bFail(cb)
			}

			ehttptest.Eq(t, "failures older than the window are not counted", cb.State().String(), "closed")
		})
	})

	t.Run("a bucket leaves alone", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			cb := bBreaker()
			for range 10 {
				_ = bFail(cb)
			}

			time.Sleep(6 * time.Second)

			for range 5 {
				_ = bFail(cb)
			}

			time.Sleep(5 * time.Second) // the first 10 are out, the 5 stay

			for range 5 {
				_ = bFail(cb)
			}

			ehttptest.Eq(t, "only the 10 failures of the old bucket rolled off", cb.State().String(), "closed")
		})
	})
}

// Many goroutines call the breaker while it moves from closed to open, to
// half-open and back. Each call gets its own result, an open breaker rejects
// the call, and nothing races (run with -race).
func TestBreakerConcurrentAcrossStateChanges(t *testing.T) {
	t.Parallel()

	cb := ehttp.NewCircuitBreaker[int](ehttp.BreakerOptions{Name: "c", Timeout: 5 * time.Millisecond})

	// Open it first, so the goroutines meet an open breaker at the start.
	for range 100 {
		_ = bFail(cb)
	}

	_, err := cb.Execute(func() (int, error) { return 0, nil })
	ehttptest.True(t, "a high failure rate trips the breaker", errors.Is(err, gobreaker.ErrOpenState), "err = %v", err)

	var (
		wg                sync.WaitGroup
		rejected, unknown atomic.Int64
	)

	for g := range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range 400 {
				// The first half of the calls fail, the second half succeed,
				// so the breaker opens and then recovers.
				fail := i < 200

				_, err := cb.Execute(func() (int, error) {
					if fail || g%2 == 0 && i%3 == 0 {
						return 0, errBoom
					}

					return 1, nil
				})

				switch {
				case err == nil || errors.Is(err, errBoom):
				case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
					rejected.Add(1)
				default:
					unknown.Add(1)
				}

				_ = cb.State()

				if i%50 == 0 {
					time.Sleep(time.Millisecond) // let the 5 ms timeout pass
				}
			}
		}()
	}

	wg.Wait()

	ehttptest.Eq(t, "gobreaker contract (only the callee error or a rejection)", unknown.Load(), int64(0))
	ehttptest.True(t, "an open breaker rejects calls", rejected.Load() > 0, "no call was rejected")

	// After the timeout a successful probe closes the breaker.
	time.Sleep(10 * time.Millisecond)

	for range 3 {
		_ = bSucceed(cb)
	}

	ehttptest.Eq(t, "gobreaker contract (a successful probe closes it)", cb.State().String(), "closed")
}
