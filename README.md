# ehttp

ehttp is a Go HTTP framework and client. Its API is familiar to users of
Express. It runs on the standard library `net/http`, so it keeps the
robustness of Go.

- Routes, route parameters, groups and middlewares, as in Express.
- Strict checks. A mistake in a route or a header panics at boot or logs a
  warning, so it does not reach production unnoticed.
- No hidden state. Each server has its own logger and its own configuration.
- A small client with timeouts, a response size limit and redacted logs.

## Requirements

ehttp needs Go 1.26.0 or later.

## Install

```
go get github.com/EqualSoftware/ehttp
```

## Quickstart

```go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
)

func main() {
	server := ehttp.NewServer(ehttp.WithPortInt(3000))

	server.Use(
		middleware.Recover(),
		middleware.Timeout(5*time.Second),
	)

	server.GET("/hello/:name", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.JSON(map[string]string{"message": "Hello, " + req.Param("name")})
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Listen(ctx); err != nil {
		panic(err)
	}
}
```

`Listen` serves until the context is done. Then it finishes the requests in
flight and returns. A clean shutdown returns `nil`.

For a longer guide, read [examples/server/main.go](examples/server/main.go).

## Routes and parameters

Register a route with `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `CONNECT` or
`TRACE`. Each method takes a pattern, a handler and optional middlewares.

- `:name` is a route parameter. Read it with `req.Param("name")`.
- `:name|regex` is a parameter that must match the regular expression. The
  regex is anchored and compiled at registration.
- `...` matches the rest of the path. Check the value before you use it as a
  file path, for example with `os.Root` or `filepath.IsLocal`.

A request that does not match returns 404. A path that matches with another
method returns 405 with an `Allow` header. An `OPTIONS` request returns the
`Allow` header.

## Groups and middleware order

`server.Use` adds middlewares to every route that is registered after the
call. Call `Use` before the first route. A call after a route panics at boot.

`server.Route(prefix, middlewares...)` makes a group. The middlewares of a
group run on the routes of the group. They also run on the 404, 405 and
`OPTIONS` answers under the prefix. A second group with the same prefix panics.

```go
admin := server.Route("/admin", basicAuth)
admin.GET("/me", handler)
```

The middlewares run in the order of the list, outside first. Use this order:

1. `middleware.Recover()`: a panic becomes a 500. Put it first.
2. `middleware.ErrorCache(...)`: sets `Cache-Control` for error responses.
   Put it outside `Timeout`, so it covers the 503 of `Timeout`.
3. `middleware.RequestId()`: sets `X-Request-Id`.
4. `middleware.Secure(middleware.DefaultSecureConfig)`: security headers.
5. `ehttp.Cors(...)`: the CORS policy. Put it before any authentication, so a
   preflight request and a refused request get the CORS headers.
6. `middleware.Timeout(...)`: a deadline on the request context.

Other middlewares are in the `middleware` package: `AcceptEncoding`,
`BodyLimit`, `Cache`, `ErrorCacheStatus`, `WeakEtag`, `LastModified` and
`Log`. Their rules are in the package documentation.

## ResponseWriter

The `*ehttp.ResponseWriter` has these methods.

| Method | Use |
| :--- | :--- |
| `Status(code)` | Set the status code. The body comes later. |
| `StatusCode()` | Read the status code that is set. |
| `SendStatus(code)` | Send a status line and no body. |
| `Send(bytes)` | Send a body of bytes. |
| `String(s)` | Send a text body. |
| `JSON(v)` | Send a JSON body. |
| `Render(name, data)` | Render a template. Set the templates with `WithTemplates`. |
| `SendFile(name)` | Send a file. It sets a strong `ETag` and answers `Range` and conditional requests. |
| `Redirect(url)` | Send a redirect. |
| `Location(url)` | Set the `Location` header. |
| `Error(err)` | Send an error response. |
| `Set(name, value)` | Replace the value of a header. |
| `Append(name, values...)` | Add members to a list header, such as `Allow`. |
| `Remove(name)` | Remove a header. |
| `Vary(names...)` | Add names to `Vary`. |
| `Cookie(cookie)` | Set a cookie. Use it for `Set-Cookie`. |
| `ClearCookie(name)` | Expire a cookie. |
| `Header()` | The header map of the response. |
| `HeadersSent()` | Report if the header was sent. |
| `Flush()`, `FlushError()` | Send the buffered body to the client. |
| `End()` | End the response. |
| `Hijack()`, `Unwrap()` | Reach the connection or the standard writer. |

`Set` and `Append` check their input. A mistake, such as an invalid name or a
call after the header was sent, panics in `Development` mode. In `Production`
mode it is logged and the header does not change. `Set-Cookie` and `Vary` have
their own methods (`Cookie` and `Vary`).

## CORS

`ehttp.DefaultCorsOptions()` allows every origin, method and header, with no
credentials. For a public API without credentials, use it as it is. For other
services, narrow the options:

```go
o := ehttp.DefaultCorsOptions()
o.Origin = []string{"https://app.example.com"}
o.Methods = []string{ehttp.MethodGet, ehttp.MethodPost}
o.CacheDuration = 5 * time.Minute

server.Use(ehttp.Cors(o))
```

Put `Cors` before any authentication middleware.

## Client

`ehttp.NewClient` makes a client. It sends a request with `Execute`.

```go
client := ehttp.NewClient(
	ehttp.WithBaseUrl("https://api.example.com"),
	ehttp.WithTimeout(5*time.Second),
)

response, err := client.Execute(
	ehttp.WithContext(ctx),
	ehttp.WithSource("GET", "/items"),
)
```

The default total timeout of a request is 30 seconds. It also covers the read
of the body.

Redirects use the standard library policy. On a redirect to another host, the
client drops `Authorization`, `WWW-Authenticate`, `Cookie`, `Cookie2`,
`Proxy-Authorization` and `Proxy-Authenticate`. A redirect to a subdomain of
the first host keeps them.

## Limits

Read these limits before you use ehttp in production.

- **Timeout is cooperative.** `middleware.Timeout` sets a deadline on the
  request context. The handler runs in the same goroutine. It must watch
  `req.Context()`. If the deadline passes and no header was sent, the response
  is 503. A handler that does not watch the context keeps running.
- **No `ClientIP`.** ehttp has no method that returns the client address. Use
  `req.Request.RemoteAddr` for the peer address. Read it through a trusted
  proxy with the guide below.
- **No `Forwarded` header.** ehttp reads only the `X-Forwarded-*` headers from
  a trusted proxy. It does not read the `Forwarded` header of RFC 7239.
- **No built-in CSRF protection.** ehttp has no CSRF middleware. Protect state
  changing requests in your application.
- **No default `ReadTimeout` or `WriteTimeout`.** The server sets
  `ReadHeaderTimeout` to 10 seconds by default. `ReadTimeout` and
  `WriteTimeout` are off by default. Set them with `WithReadTimeout` and
  `WithWriteTimeout` when you need them.
- **`WeakEtag` buffers the response.** The ETag covers the whole body, so the
  middleware keeps the body of a 200 response in memory until the handler
  ends. It buffers at most 1 MiB. A larger body is sent without an ETag.
- **One field line for `Append`.** `Append` puts all members in one field line.
  It does not add a second line for the same header. Use `Cookie` for
  `Set-Cookie`.

## Trusted proxies

ehttp trusts no proxy by default. A request from a peer that is not trusted
keeps its own scheme and host. Set the trusted peers with
`ehttp.WithTrustedProxies`:

```go
server := ehttp.NewServer(
	ehttp.WithTrustedProxies("loopback", "10.0.0.0/8"),
)
```

A value is a CIDR (`10.0.0.0/8`), a single IP (`10.0.0.1`), or one of these
names:

| Name | Ranges |
| :--- | :--- |
| `loopback` | `127.0.0.0/8`, `::1/128` |
| `linklocal` | `169.254.0.0/16`, `fe80::/10` |
| `uniquelocal` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7` |

An invalid value panics at boot. Give only the peers that you run or trust.
Do not give `0.0.0.0/0` or `::/0`.

To find the peer ranges of your platform:

- **Kubernetes:** use the pod network CIDR of the cluster, or the address
  range of the ingress controller. Your cluster administrator knows these
  values. ehttp does not publish them.
- **AWS Application Load Balancer:** the load balancer nodes send requests
  from their private addresses and add `X-Forwarded-For`, `X-Forwarded-Proto`
  and `X-Forwarded-Port`. Trust the CIDRs of the load balancer subnets. Do not
  trust the full VPC CIDR: each host in the VPC can send a false
  `X-Forwarded-For`. The file `https://ip-ranges.amazonaws.com/ip-ranges.json`
  does not list the addresses of your own load balancers.
- **AWS Network Load Balancer:** it works at layer 4 and does not add
  `X-Forwarded-*` headers. Do not add its addresses to the trusted list.
- **Google Cloud:** a regional external Application Load Balancer sends
  requests from the proxy-only subnet of the region. Trust that subnet. For
  other load balancer types, find the source ranges in the firewall rules of
  `https://docs.cloud.google.com/load-balancing/docs/tcp` and
  `https://docs.cloud.google.com/load-balancing/docs/health-check-concepts`.
- **Cloudflare:** the ranges are listed at `https://www.cloudflare.com/ips-v4/`
  and `https://www.cloudflare.com/ips-v6/`.

Ranges change. Check the source of each platform again when you deploy.

## Examples

The [server example](examples/server) is a short guide to the API. Run it from
the repository root:

```
go run ./examples/server
```

The [client example](examples/client) shows a request to an external site.

Load and stress tests are in a separate repository:
https://github.com/EqualSoftware/ehttp_benchmark.

## Development

Run the gate of the project:

```
make check
```

`make check` runs the tests, the allocation tests, the lint, the vulnerability
check and the list of known bugs. `make tdd` shows the open bugs as failures.
`make bench` runs the benchmarks in Docker.

Dependabot updates the GitHub Actions and the Go modules every week. It does
not track the tool versions that the `Makefile` pins (`golangci-lint`,
`govulncheck` and `benchstat`). Update these versions by hand.

The scheduled workflows run the nightly fuzz tests and the weekly
vulnerability check. GitHub disables scheduled workflows in a public repository
after 60 days without activity. Run the workflow by hand to keep it active.

## Security

To report a vulnerability, read [SECURITY.md](SECURITY.md). Do not open a
public issue for a vulnerability.

## License

ehttp is released under the MIT License. See [LICENSE](LICENSE).
Copyright (c) 2026 Equal Software.
