package ehttp

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker/v2"
	"go.uber.org/zap"
)

// The breaker counts the calls of a rolling window of Interval, in buckets of
// breakerBucket. It opens when the window has at least breakerMinRequests
// calls and at least breakerFailureRate of them failed. Fewer calls in one
// window never trip it. The values follow Hystrix (requestVolumeThreshold 20,
// errorThresholdPercentage 50, a 10 s rolling window),
// https://github.com/Netflix/Hystrix/wiki/Configuration, and the 50 %
// threshold of resilience4j, https://resilience4j.readme.io/docs/circuitbreaker.
const (
	breakerMinRequests = 20
	breakerFailureRate = 0.5
	breakerInterval    = 10 * time.Second
	breakerBucket      = time.Second
)

type BreakerOptions struct {
	Name string
	// Interval is the length of the rolling window of a closed breaker. Its
	// counts roll off bucket by bucket. Zero or less means 10s. The value is
	// rounded up to whole seconds (gobreaker adjusts it to a multiple of the 1 s
	// bucket). A value under 1s gives one bucket, so it is a fixed window.
	Interval        time.Duration
	Timeout         time.Duration
	PrometheusGauge *prometheus.GaugeVec
}

type CircuitBreaker[T any] = gobreaker.CircuitBreaker[T]

func NewCircuitBreaker[T any](opt BreakerOptions) *CircuitBreaker[T] {
	if opt.Interval <= 0 {
		opt.Interval = breakerInterval
	}

	cb := gobreaker.NewCircuitBreaker[T](gobreaker.Settings{
		Name:         opt.Name,
		Interval:     opt.Interval,
		BucketPeriod: min(breakerBucket, opt.Interval),
		Timeout:      opt.Timeout,
		OnStateChange: func(name string, from, to gobreaker.State) {
			if opt.PrometheusGauge != nil {
				opt.PrometheusGauge.WithLabelValues(name).Set(getCircuitBreakerStateValue(to))
			}

			zap.L().Debug(fmt.Sprintf("circuit breaker(%s) changed the state", name),
				zap.Dict("state",
					zap.String("from", from.String()),
					zap.String("to", to.String()),
				),
			)
		},
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < breakerMinRequests {
				return false
			}

			return float64(counts.TotalFailures)/float64(counts.Requests) >= breakerFailureRate
		},
	})

	if opt.PrometheusGauge != nil {
		opt.PrometheusGauge.WithLabelValues(opt.Name).Set(getCircuitBreakerStateValue(cb.State()))
	}

	return cb
}

func getCircuitBreakerStateValue(state gobreaker.State) float64 {
	switch state {
	case gobreaker.StateClosed:
		return 0
	case gobreaker.StateHalfOpen:
		return 1
	case gobreaker.StateOpen:
		return 2
	default:
		return -1
	}
}
