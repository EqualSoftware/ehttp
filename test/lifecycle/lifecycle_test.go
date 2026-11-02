package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

// ---------------------------------------------------------------------------
// Lifecycle tests: always a real server.
// ---------------------------------------------------------------------------

// cTimeoutServer wires Timeout (and optionally an outer probe) around h.
func cTimeoutServer(t testing.TB, d time.Duration, h ehttp.HandlerFunc, outer ...ehttp.MiddlewareHandlerFunc) *httptest.Server {
	srv := ehttp.NewServer()
	srv.Use(outer...)
	srv.Use(middleware.Timeout(d))
	srv.GET("/t", h)

	return fixtures.Start(t, srv)
}

// The timeout is cooperative (as Echo ContextTimeout and chi).
// The handler runs in the request goroutine. Timeout only sets the deadline on
// the context. A handler that ignores the context keeps its response, because
// a header is sent before Timeout checks the deadline.
func TestCLifecycleTimeoutNonCooperativeHandler(t *testing.T) {
	const timeout = 30 * time.Millisecond

	done := make(chan error, 1)

	ts := cTimeoutServer(t, timeout, func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		time.Sleep(3 * timeout) // ignores the context on purpose
		done <- req.Context().Err()
		res.String("late")
	})

	resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/t", nil)

	ehttptest.Eq(t, "cooperative timeout: a handler that ignores the context sends its late response", resp.StatusCode, http.StatusOK)
	ehttptest.Eq(t, "cooperative timeout: a handler that ignores the context sends its late response", body, "late")

	select {
	case err := <-done:
		ehttptest.True(t, "the handler context carries the deadline",
			errors.Is(err, context.DeadlineExceeded), "handler ctx err = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("handler never finished")
	}
}

func TestCLifecycleTimeoutStatus(t *testing.T) {
	ts := cTimeoutServer(t, 30*time.Millisecond, func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		<-req.Context().Done()
	})

	start := time.Now()
	resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/t", nil)

	ehttptest.True(t, "answers at the deadline without waiting for the handler",
		time.Since(start) < 2*time.Second, "took %v", time.Since(start))

	// 408 means the CLIENT was slow (RFC 9110 §15.5.9); a server timeout is 503 (§15.6.4).
	ehttptest.Eq(t, "RFC 9110 §15.6.4 / http.TimeoutHandler", resp.StatusCode, http.StatusServiceUnavailable)
}

// The Timeout middleware overwrites req.Request in place; outer middlewares
// must keep a live context after it returns.
func TestCLifecycleTimeoutRestoresOuterContext(t *testing.T) {
	seen := make(chan error, 1)

	outer := func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			next.ServeHTTP(res, req)
			seen <- req.Context().Err()
		}
	}

	ts := cTimeoutServer(t, time.Second, func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String("ok")
	}, outer)

	resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/t", nil)
	ehttptest.Eq(t, "happy path", resp.StatusCode, http.StatusOK)
	ehttptest.Eq(t, "happy path", body, "ok")

	err := <-seen

	ehttptest.True(t, "Timeout restores req.Request for outer middlewares",
		err == nil, "outer middleware sees context error after Timeout: %v", err)
}

// A panic in the goroutine created by Timeout is outside Recover and outside
// net/http's recover: it kills the process. Only a child process can observe it.
func TestCLifecycleTimeoutPanicKillsProcess(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCChildTimeoutPanic$", "-test.count=1")
	cmd.Env = append(os.Environ(), "EHTTP_C_CHILD=timeout-panic")

	out, err := cmd.CombinedOutput()
	survived := strings.Contains(string(out), "CHILD-SURVIVED status=500")

	ehttptest.True(t, "Recover outside Timeout catches the handler panic; the process survives",
		survived && err == nil, "child died (err=%v)", err)
}

// TestCChildTimeoutPanic runs only inside the child process of the test above.
func TestCChildTimeoutPanic(t *testing.T) {
	if os.Getenv("EHTTP_C_CHILD") != "timeout-panic" {
		return
	}

	srv := ehttp.NewServer()
	srv.Use(middleware.Recover(), middleware.Timeout(time.Second))
	srv.GET("/p", func(res *ehttp.ResponseWriter, req *ehttp.Request) { panic("boom") })

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/p", http.NoBody)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("CHILD-ERR", err)
		return
	}

	resp.Body.Close()
	fmt.Printf("CHILD-SURVIVED status=%d\n", resp.StatusCode)
}

func TestCLifecycleClientDisconnectCancelsContext(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)

	srv := ehttp.NewServer()
	srv.GET("/wait", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		close(started)
		<-req.Context().Done()
		cancelled <- req.Context().Err()
	})

	ts := fixtures.Start(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/wait", http.NoBody)
	errc := make(chan error, 1)

	go func() {
		resp, err := fixtures.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}

		errc <- err
	}()

	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("request ended before the handler started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel() // client goes away

	select {
	case err := <-cancelled:
		ehttptest.True(t, "net/http: server cancels r.Context() when the client disconnects",
			errors.Is(err, context.Canceled), "ctx err = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("handler context was not cancelled after client disconnect")
	}

	<-errc //nolint:errcheck // the test only waits for Listen to return
}

func TestCLifecycleRecoverKeepsConnectionAlive(t *testing.T) {
	var conns atomic.Int32

	srv := ehttp.NewServer()
	srv.Use(middleware.Recover())
	srv.GET("/panic", func(res *ehttp.ResponseWriter, req *ehttp.Request) { panic("boom") })
	srv.GET("/ok", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("ok") })

	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}

	for _, tc := range []struct {
		path string
		want int
	}{{"/panic", 500}, {"/ok", 200}, {"/panic", 500}, {"/ok", 200}} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+tc.path, http.NoBody)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ehttptest.Eq(t, "panic becomes 500 (path "+tc.path+")", resp.StatusCode, tc.want)
	}

	ehttptest.Eq(t, "RFC 9112 §9.3: keep-alive survives a recovered panic (one connection)", conns.Load(), int32(1))
}

// ---------------------------------------------------------------------------
// Listen / Shutdown
// ---------------------------------------------------------------------------

func TestCLifecycleListenCancelStopsServer(t *testing.T) {
	l := fixtures.Listen(t, func(port string) *ehttp.Server {
		srv := ehttp.NewServer(ehttp.WithPort(port))
		srv.GET("/ok", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("ok") })

		return srv
	})

	resp, body := fixtures.Fetch(t, http.MethodGet, "http://127.0.0.1:"+l.Port+"/ok", nil)
	ehttptest.Eq(t, "Listen serves", resp.StatusCode, 200)
	ehttptest.Eq(t, "Listen serves", body, "ok")

	l.Cancel()

	select {
	case err := <-l.Result:
		ehttptest.Eq(t, "clean cancel returns nil", err, nil)
	case <-time.After(3 * time.Second):
		t.Fatal("Listen did not return after ctx cancel")
	}

	_, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", "127.0.0.1:"+l.Port)
	ehttptest.True(t, "listener is closed after shutdown", err != nil, "port still accepts")
}

func TestCLifecycleListenReturnsBindError(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	port := fmt.Sprint(busy.Addr().(*net.TCPAddr).Port)
	srv := ehttp.NewServer(ehttp.WithPort(port))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := make(chan error, 1)

	go func() { got <- srv.Listen(ctx) }()

	var listenErr error

	select {
	case listenErr = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("Listen hung on a busy port")
	}

	ehttptest.True(t, "bind failure must be returned, not swallowed",
		listenErr != nil, "Listen returned nil on busy port %s", port)
}

func TestCLifecycleListenShutdownWaitsForInflight(t *testing.T) {
	var finished atomic.Bool

	started := make(chan struct{}, 1)

	l := fixtures.Listen(t, func(port string) *ehttp.Server {
		srv := ehttp.NewServer(ehttp.WithPort(port))
		srv.GET("/slow", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			started <- struct{}{}
			time.Sleep(300 * time.Millisecond)
			finished.Store(true)
			res.String("done")
		})

		return srv
	})

	// The goroutine must not call t.Fatal: it sends the error to the test goroutine.
	clientDone := make(chan error, 1)

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+l.Port+"/slow", http.NoBody)
		if err != nil {
			clientDone <- err
			return
		}

		resp, err := fixtures.Client().Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		clientDone <- err
	}()

	select {
	case <-started:
	case err := <-clientDone:
		t.Fatalf("request ended before the handler started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	l.Cancel()

	var listenErr error

	select {
	case listenErr = <-l.Result:
	case <-time.After(3 * time.Second):
		t.Fatal("Listen did not return")
	}

	returnedBeforeHandler := !finished.Load()
	_ = listenErr

	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("in-flight request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never got its response")
	}

	ehttptest.True(t, "graceful shutdown waits for in-flight requests (net/http Server.Shutdown)",
		!returnedBeforeHandler, "Listen returned while the handler was still running")
}

// ---------------------------------------------------------------------------
// HEAD on the real wire
// ---------------------------------------------------------------------------

func TestCLifecycleHeadHasNoBodyOnTheWire(t *testing.T) {
	srv := ehttp.NewServer()
	srv.GET("/hello", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("hello") })

	ts := fixtures.Start(t, srv)
	addr := strings.TrimPrefix(ts.URL, "http://")

	get := fixtures.Raw(t, addr, "GET /hello HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	head := fixtures.Raw(t, addr, "HEAD /hello HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")

	headHead, headBody, _ := strings.Cut(head, "\r\n\r\n")
	getHead, getBody, _ := strings.Cut(get, "\r\n\r\n")

	ehttptest.Eq(t, "sanity: GET carries the body", getBody, "hello")
	ehttptest.Eq(t, "RFC 9110 §9.3.2: HEAD response has no content", headBody, "")
	ehttptest.True(t, "RFC 9110 §9.3.2: HEAD keeps GET's Content-Length",
		strings.Contains(headHead, "Content-Length: 5") && strings.Contains(getHead, "Content-Length: 5"),
		"HEAD headers: %q", headHead)
	ehttptest.True(t, "RFC 9110 §9.3.2: HEAD status line is 200", strings.HasPrefix(head, "HTTP/1.1 200"), "%q", head)
}

// ---------------------------------------------------------------------------
// Server timeouts over a real Listen
// ---------------------------------------------------------------------------

func TestCLifecycleReadHeaderTimeoutDropsSlowClient(t *testing.T) {
	l := fixtures.Listen(t, func(port string) *ehttp.Server {
		return ehttp.NewServer(ehttp.WithPort(port), ehttp.WithReadHeaderTimeout(200*time.Millisecond))
	})

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", "127.0.0.1:"+l.Port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nX-Slow: "))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	_, readErr := conn.Read(make([]byte, 1))

	var nerr net.Error

	closed := readErr != nil && (!errors.As(readErr, &nerr) || !nerr.Timeout())
	ehttptest.True(t, "WithReadHeaderTimeout drops a connection that never ends its headers", closed, "connection still open: %v", readErr)
}

func TestCLifecycleShutdownTimeoutStopsWaiting(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	l := fixtures.Listen(t, func(port string) *ehttp.Server {
		srv := ehttp.NewServer(ehttp.WithPort(port), ehttp.WithShutdownTimeout(100*time.Millisecond))
		srv.GET("/hang", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			started <- struct{}{}
			<-release
		})

		return srv
	})
	defer close(release)

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+l.Port+"/hang", http.NoBody)

		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	<-started
	l.Cancel()

	select {
	case err := <-l.Result:
		ehttptest.True(t, "Listen returns the deadline error", errors.Is(err, context.DeadlineExceeded), "got %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Listen ignored WithShutdownTimeout")
	}
}
