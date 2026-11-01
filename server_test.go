package ehttp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aFreePort returns a TCP port that was free a moment ago.
func aFreePort(t *testing.T) string {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// aWaitReady polls url until it answers or the deadline passes.
func aWaitReady(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)

		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("server at %s never became ready", url)
}

// aRestoreCors puts the global DefaultCorsOptions back after a test that
// mutates it (S3). Tests that call WithCors must not be parallel.
func aRestoreCors(t *testing.T) {
	t.Helper()

	saved := *DefaultCorsOptions

	t.Cleanup(func() { *DefaultCorsOptions = saved })
}

func TestNewServerDefaults(t *testing.T) {
	t.Parallel()

	s := NewServer()

	ehttptest.Eq(t, "Express app.listen examples use 3000", s.port, "3000")
	ehttptest.True(t, "default CORS allows everything", len(s.router.cors.Origin) == 1 && s.router.cors.Origin[0] == "*", "got %v", s.router.cors.Origin)

	t.Run("server gets its own CORS options", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			ehttptest.True(t, "S3: NewServer must not share the DefaultCorsOptions pointer", s.router.cors != DefaultCorsOptions, "router.cors is the global")
		})
	})
}

func TestServerPortOptions(t *testing.T) {
	t.Parallel()

	ehttptest.Eq(t, "WithPort", NewServer(WithPort("8080")).port, "8080")
	ehttptest.Eq(t, "WithPortInt", NewServer(WithPortInt(9090)).port, "9090")
	ehttptest.Eq(t, "last option wins", NewServer(WithPort("1"), WithPortInt(2)).port, "2")
}

func TestServerWithTemplates(t *testing.T) {
	t.Parallel()

	s := aTemplateServer(t, map[string]string{"a.html": `A:{{.}}`, "b.html": `B:{{.}}`})
	s.GET("/a", func(res *ResponseWriter, _ *Request) { res.Render("a.html", 1) })
	s.GET("/b", func(res *ResponseWriter, _ *Request) { res.Render("b.html", 2) })

	ehttptest.Eq(t, "Express app.set('views')", aDo(s, "GET", "/a").Body.String(), "A:1")
	ehttptest.Eq(t, "Express app.set('views')", aDo(s, "GET", "/b").Body.String(), "B:2")
}

// zap's Fatal calls os.Exit(1) even when the logger is a no-op, so the invalid
// glob has to run in a child process. The child is this same test binary.
func TestServerWithTemplatesInvalidGlob(t *testing.T) {
	if glob := os.Getenv("EHTTP_A_CHILD_GLOB"); glob != "" {
		NewServer(WithTemplates(glob))
		return // only reached when the option neither exits nor panics
	}

	cases := map[string]string{
		"malformed pattern":    "[",
		"pattern with no file": filepath.Join(t.TempDir(), "*.html"),
	}

	for name, glob := range cases {
		t.Run(name, func(t *testing.T) {
			run := func() (output string, exitCode int) {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestServerWithTemplatesInvalidGlob$", "-test.count=1")
				cmd.Env = append(os.Environ(), "EHTTP_A_CHILD_GLOB="+glob)
				out, err := cmd.CombinedOutput()

				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return string(out), exitErr.ExitCode()
				}

				if err != nil {
					t.Fatal(err)
				}

				return string(out), 0
			}

			out, code := run()

			ehttptest.True(t, "principle 5: bad config stops the boot", code != 0, "child exited 0, output: %s", out)

			t.Run("fails with a panic", func(t *testing.T) {
				ehttptest.KnownBug(t, "NA-4", func(t testing.TB) {
					ehttptest.True(t, "principle 5: panic at boot (recoverable, testable), not os.Exit", strings.Contains(out, "panic:"),
						"child exit code %d without a panic, output: %q", code, out)
				})
			})
		})
	}
}

// WithCors tests mutate the global DefaultCorsOptions (S3), so they are serial.
func TestServerWithCors(t *testing.T) {
	aRestoreCors(t)

	t.Run("empty fields keep the defaults", func(t *testing.T) {
		aRestoreCors(t)

		s := NewServer(WithCors(CorsOptions{Origin: []string{"https://a.example"}}))
		rec := aDo(s, "GET", "/x", HeaderOrigin, "https://a.example")

		ehttptest.Eq(t, "WithCors partial options", rec.Header().Get(HeaderAccessControlAllowMethods), "*")
		ehttptest.Eq(t, "WithCors partial options", rec.Header().Get(HeaderAccessControlAllowHeaders), "*")
	})

	t.Run("Credentials is taken as given", func(t *testing.T) {
		aRestoreCors(t)

		s := NewServer(WithCors(CorsOptions{Credentials: true}))
		rec := aDo(s, "GET", "/x")

		ehttptest.Eq(t, "WithCors", rec.Header().Get(HeaderAccessControlAllowCredentials), "true")
	})

	t.Run("a second server does not inherit the first one's CORS options", func(t *testing.T) {
		aRestoreCors(t)

		NewServer(WithCors(CorsOptions{Methods: []string{"GET"}, Credentials: true}))
		second := NewServer()
		rec := aDo(second, "GET", "/x")

		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			ehttptest.Eq(t, "S3: WithCors must not mutate DefaultCorsOptions", rec.Header().Get(HeaderAccessControlAllowMethods), "*")
			ehttptest.Eq(t, "S3: WithCors must not mutate DefaultCorsOptions", rec.Header().Get(HeaderAccessControlAllowCredentials), "false")
		})
	})

	t.Run("CacheDuration reaches the preflight", func(t *testing.T) {
		aRestoreCors(t)

		s := NewServer(WithCors(CorsOptions{CacheDuration: time.Hour}))
		rec := aDo(s, "OPTIONS", "/x", HeaderOrigin, "https://a.example", HeaderAccessControlRequestMethod, "GET")

		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			ehttptest.True(t, "S3: WithCors copies CacheDuration", strings.Contains(rec.Header().Get(HeaderCacheControl), "max-age=3600"),
				"Cache-Control: %q", rec.Header().Get(HeaderCacheControl))
		})
	})

	t.Run("CacheDuration becomes Access-Control-Max-Age", func(t *testing.T) {
		aRestoreCors(t)

		s := NewServer(WithCors(CorsOptions{CacheDuration: time.Hour}))
		rec := aDo(s, "OPTIONS", "/x", HeaderOrigin, "https://a.example", HeaderAccessControlRequestMethod, "GET")

		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			ehttptest.Eq(t, "S3 / Fetch §3.2.2 (preflight cache)", rec.Header().Get(HeaderAccessControlMaxAge), "3600")
		})
	})
}

func TestServerVerbDelegation(t *testing.T) {
	t.Parallel()

	s := NewServer()
	reg := map[string]func(string, HandlerFunc, ...MiddlewareHandlerFunc){
		"GET": s.GET, "POST": s.POST, "PUT": s.PUT, "PATCH": s.PATCH,
		"DELETE": s.DELETE, "CONNECT": s.CONNECT, "TRACE": s.TRACE,
	}

	for method, register := range reg {
		register("/verb/"+method, aText(method))
	}

	for method := range reg {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			ehttptest.Eq(t, "RFC 9110 §9.3", aDo(s, method, "/verb/"+method).Body.String(), method)
		})
	}
}

// aListen runs s.Listen on a free port and returns its URL and result channel.
func aListen(t *testing.T, s *Server, port string, ctx context.Context) <-chan error {
	t.Helper()

	s.port = port
	errc := make(chan error, 1)

	go func() { errc <- s.Listen(ctx) }()

	return errc
}

func TestServerListenServesUntilContextIsDone(t *testing.T) {
	port := aFreePort(t)
	s := NewServer()
	s.GET("/ping", aText("pong"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := aListen(t, s, port, ctx)
	url := "http://127.0.0.1:" + port
	aWaitReady(t, url+"/ping")

	resp, body := aGetBody(t, url+"/ping")
	ehttptest.Eq(t, "Listen serves the router", resp.StatusCode, 200)
	ehttptest.Eq(t, "Listen serves the router", body, "pong")

	cancel()

	select {
	case err := <-errc:
		ehttptest.Eq(t, "Listen returns nil after a graceful stop", err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after the context was canceled")
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/ping", http.NoBody)

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	ehttptest.True(t, "listener is closed after shutdown", err != nil, "server still accepting")
}

func TestServerListenReturnsBindError(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errc := aListen(t, NewServer(), port, ctx)

	var got error

	select {
	case got = <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen hung on a busy port")
	}

	ehttptest.KnownBug(t, "S8", func(t testing.TB) {
		ehttptest.True(t, "S8: Listen returns the real error", got != nil, "Listen returned nil for a busy port")
	})
}

func TestServerListenWaitsForInFlightRequests(t *testing.T) {
	port := aFreePort(t)
	s := NewServer()
	s.GET("/ping", aText("pong"))

	started := make(chan struct{})
	release := make(chan struct{})

	var (
		finished atomic.Bool
		once     sync.Once
	)

	s.GET("/slow", func(res *ResponseWriter, _ *Request) {
		close(started)
		<-release
		finished.Store(true)
		res.String("done")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := aListen(t, s, port, ctx)
	url := "http://127.0.0.1:" + port
	aWaitReady(t, url+"/ping")

	type result struct {
		body string
		err  error
	}

	clientDone := make(chan result, 1)

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/slow", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			clientDone <- result{err: err}
			return
		}
		defer resp.Body.Close()

		b := new(bytes.Buffer)
		_, err = b.ReadFrom(resp.Body)
		clientDone <- result{body: b.String(), err: err}
	}()

	<-started

	// If Listen waits for the request, the handler ends after this delay.
	timer := time.AfterFunc(200*time.Millisecond, func() { once.Do(func() { close(release) }) })
	defer timer.Stop()

	cancel()

	select {
	case <-errc: //nolint:errcheck // the test only waits for Listen to return
	case <-time.After(10 * time.Second):
		t.Fatal("Listen did not return")
	}

	handlerFinishedBeforeListenReturned := finished.Load()

	once.Do(func() { close(release) })

	select {
	case r := <-clientDone:
		ehttptest.Eq(t, "in-flight request still completes", r.err, nil)
		ehttptest.Eq(t, "in-flight request still completes", r.body, "done")
	case <-time.After(5 * time.Second):
		t.Fatal("client never got its response")
	}

	ehttptest.KnownBug(t, "S8", func(t testing.TB) {
		ehttptest.True(t, "S8: shutdown waits for in-flight requests (up to a deadline)", handlerFinishedBeforeListenReturned,
			"Listen returned while the handler was still running")
	})
}
