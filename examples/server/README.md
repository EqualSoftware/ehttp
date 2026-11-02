# Example server

`main.go` runs the server on port 3000 (`go run ./examples/server`).
It has a group `/admin` with HTTP Basic auth (`demo` / `demo-password`, for the
demo only), a group `/api/v1`, CORS for `http://localhost:3000`, and trusted
proxies (only the nginx address of the compose file).

## Experiment stack

Needs Docker. From the repository root:

```
docker compose -f examples/server/compose.yaml up --build -d
docker compose -f examples/server/compose.yaml down        # stop and remove
```

| Service    | URL                      | Note |
| :--------- | :----------------------- | :--- |
| nginx      | http://localhost:8080    | Trusted proxy. Sets `X-Forwarded-For`, `-Proto` and `-Host`; the example uses them. |
| example    | http://localhost:3000    | Direct. Not trusted: `X-Forwarded-*` from the client are ignored. |
| Prometheus | http://localhost:9090    | Scrapes `example:3000/metrics` every 5 s. |
| Grafana    | http://localhost:3001    | Login `admin` / `admin` (demo only). Dashboards "ehttp example" and "Containers (cAdvisor)" are provisioned. |
| cAdvisor   | (internal)               | Container CPU and memory, scraped by Prometheus. |

The app has limits (1 CPU, 256 MB) and nginx too (0.5 CPU, 64 MB); the Containers dashboard shows use against them.

Load and stress tests (k6) and the comparison with other frameworks are in a separate repository: https://github.com/EqualSoftware/ehttp_benchmark. Its stress tests target this stack (nginx on 127.0.0.1:8080); watch the two dashboards while they run to see the effect on the app and on the containers.

To open the circuit breaker, send failures: `for i in $(seq 30); do curl -s -o /dev/null "localhost:8080/flaky?fail=1"; done`.

## Cache-Control

The app has `middleware.ErrorCache(99*time.Second)` as a global middleware, outside `Timeout`.

- 404, 405, 410, 414 and 501 get `max-age=99, stale-while-revalidate=99, stale-if-error=99`.
- 428, 429, 431 and 511 get `no-store`.
- Each other error status gets `no-store`. This stops cache-poisoned DoS (CPDoS).
- 401, 403, 407 and a request with `Authorization` get `private` when a lifetime applies.
- A `Cache-Control` that a handler sets wins.

Use `middleware.ErrorCacheStatus(code, d)` to change one status. Use `middleware.Cache(d)` on a route for the statuses below 400.
The rules, the RFC basis and runnable examples are in the doc comments of `ErrorCache`, `ErrorCacheStatus` and `Cache` (package `middleware`, `example_test.go`).

## Metrics

The library exposes one metric: the gauge of the circuit breaker
(`BreakerOptions.PrometheusGauge`). It has no request metrics. For the
dashboard, `app/metrics.go` adds a request counter and a latency histogram in
the example only. `app.NewObserved` turns them on (and `/metrics`, `/flaky`);
`app.New` does not, so the golden and perf suites are not affected.

## macOS

Docker runs in a Linux VM: cAdvisor sees the VM, not the Mac, and the CPU and memory numbers depend on the VM size. Do not compare load or benchmark numbers with runs
from another environment.
