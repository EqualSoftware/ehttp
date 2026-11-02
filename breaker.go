package ehttp

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker/v2"
	"go.uber.org/zap"
)

// The breaker opens when at least breakerMinRequests calls ran in the
// interval and at least breakerFailureRate of them failed. The values follow
// Hystrix (requestVolumeThreshold 20, errorThresholdPercentage 50, a 10 s
// rolling window), https://github.com/Netflix/Hystrix/wiki/Configuration, and
// the 50 % threshold of resilience4j,
// https://resilience4j.readme.io/docs/circuitbreaker.
const (
	breakerMinRequests = 20
	breakerFailureRate = 0.5
	breakerInterval    = 10 * time.Second
)

type BreakerOptions struct {
	Name string
	// Interval is the period after which the counts of a closed breaker reset.
	// Default 10s.
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
		Name:     opt.Name,
		Interval: opt.Interval,
		Timeout:  opt.Timeout,
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
