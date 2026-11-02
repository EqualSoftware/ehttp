package ehttp_test

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/prometheus/client_golang/prometheus"
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
