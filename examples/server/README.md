# Example server

A short guide to ehttp. Read `main.go` from top to bottom. Run it from the
repository root:

```
go run ./examples/server
```

The server listens on port 3000. Press Ctrl+C to stop it: `Listen` finishes
the requests in flight, then returns (graceful shutdown).

The curl commands below use `-i` to show the status line and the headers.

## 1. Basic: routes, parameters, JSON, status codes

| Route               | What it shows                                   |
| :------------------ | :---------------------------------------------- |
| `GET /hello/:name`  | A route parameter (`req.Param`) and a JSON body |
| `POST /items`       | `res.Status(201)` before the body               |
| `GET /teapot`       | `res.SendStatus(418)`: a status and no body     |

```
curl -i localhost:3000/hello/world        # 200 {"message":"Hello, world"}
curl -i -X POST localhost:3000/items      # 201
curl -i localhost:3000/teapot             # 418
curl -i localhost:3000/missing            # 404
curl -i -X DELETE localhost:3000/items    # 405 with an Allow header
```

## 2. Middlewares

`server.Use` takes the middlewares in the recommended order, outside first:

1. `Recover`: a panic becomes a 500.
2. `ErrorCache`: sets `Cache-Control` for error responses. It is outside
   `Timeout`, so it covers the 503 of `Timeout`.
3. `RequestId`: sets `X-Request-Id`.
4. `Secure`: security headers.
5. `Cors`: the CORS policy. It comes before any authentication, so a
   preflight and a refusal get the CORS headers. `DefaultCorsOptions()` allows
   everything; the example narrows it to one origin.
6. `Timeout`: a deadline of 1 second on the request context. It is
   cooperative: the handler of `/slow` watches `req.Context()`.

```
curl -i localhost:3000/hello/world        # X-Request-Id and the security headers
curl -i localhost:3000/boom               # 500: Recover caught the panic
curl -i localhost:3000/slow               # 503 after 1 second: Timeout
curl -i -X OPTIONS localhost:3000/items \
  -H 'Origin: http://localhost:5173' \
  -H 'Access-Control-Request-Method: POST'  # CORS preflight: allowed
curl -i -X OPTIONS localhost:3000/items \
  -H 'Origin: http://evil.example' \
  -H 'Access-Control-Request-Method: POST'  # CORS preflight: refused
```

## 3. A group with Basic auth

`server.Route("/admin", basicAuth(...))` makes a group. The credentials
`demo` / `demo-password` are for the demo only.

The middleware of a group also runs on the 401, 404, 405 and OPTIONS answers
under its prefix. So only a group with auth hides the `Allow` header from a
client without credentials. A route outside the group shows it.

```
curl -i localhost:3000/admin/me                          # 401 with WWW-Authenticate
curl -i -u demo:demo-password localhost:3000/admin/me    # 200
curl -i -X OPTIONS localhost:3000/admin/me               # 401: no Allow header
curl -i -X OPTIONS -u demo:demo-password localhost:3000/admin/me   # 200 with Allow
curl -i -X OPTIONS localhost:3000/hello/world            # 200 with Allow
```

## 4. Files and cache

`GET /notes.txt` uses `res.SendFile`. It sets a strong `ETag`, and it answers
`Range` and conditional requests. The route uses `middleware.Cache(time.Minute)`
for the `Cache-Control` of the success responses. The `ErrorCache` of section 2
does the same for errors.

```
curl -i localhost:3000/notes.txt                                  # 200, ETag, Cache-Control
curl -i -H 'Range: bytes=0-49' localhost:3000/notes.txt           # 206, first 50 bytes
curl -i -H 'If-None-Match: "<the ETag value>"' localhost:3000/notes.txt   # 304
```

Load and stress tests (k6) and the comparison with other frameworks are in a separate repository: https://github.com/EqualSoftware/ehttp_benchmark.
