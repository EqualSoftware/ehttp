package app

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The library has no request metrics (author decision: none in the library). It exposes only the gauge of the
// circuit breaker (BreakerOptions.PrometheusGauge). This file adds a request
// counter and a latency histogram for the example only, in its own registry.
// New does not use it, so the golden and perf suites do not pay for it.

var errDemoFailure = errors.New("demo failure")

// observe registers the metrics. It returns the middleware that counts each
// request and a function that adds the routes. Call that function after Use:
// a middleware added after a route panics.
func observe(reg *prometheus.Registry) (middleware ehttp.MiddlewareHandlerFunc, routes func(*ehttp.Server)) {
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "Requests by route, method and status code.",
	}, []string{"route", "method", "code"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds", Help: "Request duration by route.", Buckets: prometheus.DefBuckets,
	}, []string{"route"})
	breakerState := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "circuit_breaker_state", Help: "Circuit breaker state: 0 closed, 1 half-open, 2 open.",
	}, []string{"name"})
	bytesIn := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_request_bytes_total", Help: "Request body bytes, from Content-Length.",
	})
	bytesOut := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_response_bytes_total", Help: "Response body bytes, from Content-Length (not set for a chunked body).",
	})
	reg.MustRegister(requests, latency, breakerState, bytesIn, bytesOut)

	routes = func(server *ehttp.Server) {
		server.GET("/metrics", ehttp.WrapHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))
		flaky(server, breakerState)
	}

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			start := time.Now()
			next.ServeHTTP(res, req)

			route := req.RoutePath()
			if route == "" {
				route = "unmatched" // a bounded label: never the raw path
			}

			requests.WithLabelValues(route, req.Method(), strconv.Itoa(res.StatusCode())).Inc()
			latency.WithLabelValues(route).Observe(time.Since(start).Seconds())

			// ResponseWriter has no byte counter: use the declared lengths.
			bytesIn.Add(float64(max(req.Request.ContentLength, 0)))
			if n, err := strconv.ParseFloat(res.Header().Get(ehttp.HeaderContentLength), 64); err == nil {
				bytesOut.Add(n)
			}
		}
	}, routes
}

// flaky adds GET /flaky?fail=1 behind a circuit breaker, so a load test can
// open the breaker and the dashboard shows its state.
func flaky(server *ehttp.Server, gauge *prometheus.GaugeVec) {
	cb := ehttp.NewCircuitBreaker[string](ehttp.BreakerOptions{
		Name: "flaky", Timeout: 5 * time.Second, PrometheusGauge: gauge,
	})

	server.GET("/flaky", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		fail := req.Query("fail") != ""

		out, err := cb.Execute(func() (string, error) {
			if fail {
				return "", errDemoFailure
			}

			return "ok\n", nil
		})
		if err != nil {
			res.SendStatus(http.StatusServiceUnavailable)
			return
		}

		res.String(out)
	})
}
