package ehttp

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker/v2"
	"go.uber.org/zap"
)

type BreakerOptions struct {
	Name            string
	Interval        time.Duration
	Timeout         time.Duration
	PrometheusGauge *prometheus.GaugeVec
}

type CircuitBreaker[T any] = gobreaker.CircuitBreaker[T]

func NewCircuitBreaker[T any](opt BreakerOptions) *CircuitBreaker[T] {
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
			ratio := float64(counts.ConsecutiveFailures) / float64(counts.ConsecutiveSuccesses)
			return ratio >= 0.5
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
