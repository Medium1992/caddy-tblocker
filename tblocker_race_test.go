package tblocker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// A ban can land at any moment, including between the handler's ban check and
// the moment it registers itself as cancellable. Whichever side wins, the
// request must not be left running: either it is refused up front, or it is
// torn down. Anything else means a live tunnel silently survives its ban.
func TestBanNeverLosesARequestToTheRegistrationWindow(t *testing.T) {
	const attempts = 3000
	addr := netip.MustParseAddr("203.0.113.42")
	survived := 0

	for i := 0; i < attempts; i++ {
		now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
		app := newTestApp(now)
		handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

		var wg sync.WaitGroup
		outcome := make(chan string, 1)

		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			_ = handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
				caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
					select {
					case <-r.Context().Done():
						outcome <- "cancelled"
					case <-time.After(2 * time.Second):
						outcome <- "survived"
					}
					return nil
				}))
			if recorder.Code == http.StatusForbidden {
				select {
				case outcome <- "refused":
				default:
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			app.Ban(addr, now.Add(time.Minute))
		}()

		wg.Wait()
		select {
		case got := <-outcome:
			if got == "survived" {
				survived++
			}
		default:
			// The handler refused the request before reaching next.
		}
		if survived > 0 {
			t.Fatalf("iteration %d: a request outlived its ban", i)
		}
	}
}

// The same invariant with many concurrent requests from one address.
func TestBanTearsDownEveryConcurrentRequest(t *testing.T) {
	const requests = 64
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

	var wg sync.WaitGroup
	entered := make(chan struct{}, requests)
	survived := make(chan struct{}, requests)

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			_ = handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
				caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
					entered <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
						survived <- struct{}{}
					}
					return nil
				}))
		}()
	}

	for i := 0; i < requests; i++ {
		<-entered
	}
	if _, dropped := app.Ban(addr, now.Add(time.Minute)); dropped != requests {
		t.Fatalf("dropped=%d want=%d", dropped, requests)
	}
	wg.Wait()
	if len(survived) != 0 {
		t.Fatalf("%d requests outlived the ban", len(survived))
	}
}

// A report may carry an absurd blockDuration, whether by accident or on
// purpose. Multiplying it into a time.Duration overflows int64 at roughly
// 9.2e9 seconds, which would silently turn the ban into a past timestamp and
// let the client through.
func TestAbsurdBlockDurationStillProducesALiveBan(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for _, duration := range []int64{
		1 << 34,    // overflows once multiplied into nanoseconds
		1 << 62,    // overflows once multiplied by a second
		9223372036, // just under the overflow threshold
		9223372037, // just over it
		1<<63 - 1,  // int64 max
	} {
		app := newTestApp(now)
		webhook := newTestWebhook(app)
		body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":` +
			formatInt(duration) + `}}`

		if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
			t.Fatalf("blockDuration=%d status=%d", duration, recorder.Code)
		}
		entries := app.Bans()
		if len(entries) != 1 {
			t.Fatalf("blockDuration=%d produced no live ban", duration)
		}
		if want := now.Add(time.Duration(app.MaxTTL)); !entries[0].ExpiresAt.Equal(want) {
			t.Fatalf("blockDuration=%d expiresAt=%s want=%s (clamped to max_ttl)",
				duration, entries[0].ExpiresAt, want)
		}
	}
}

// A header name has to be a valid RFC 9110 token, otherwise the block response
// carries a malformed field that proxies and clients may reject.
func TestValidateHeaderName(t *testing.T) {
	for _, name := range []string{"WWW-Authenticate", "X-Foo", "Cache-Control", "x_custom", "A1"} {
		if err := validateHeaderName(name); err != nil {
			t.Fatalf("%q must be accepted: %v", name, err)
		}
	}
	for _, name := range []string{"", "X Bad", "X:Bad", "X\nBad", "X\rBad", "X\tBad", "Bad/Name", "Bad@Name", "Bad(Name)"} {
		if err := validateHeaderName(name); err == nil {
			t.Fatalf("%q must be rejected", name)
		}
	}
}

// Provision is the path a JSON config takes, so the check has to fire there
// too, and the message has to name the offending header.
func TestProvisionRejectsInvalidHeaderName(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: newBackground()})
	defer cancel()

	handler := Handler{StatusCode: http.StatusForbidden, Headers: map[string][]string{"X Bad": {"v"}}}
	err := handler.Provision(ctx)
	if err == nil || !strings.Contains(err.Error(), "X Bad") {
		t.Fatalf("err=%v, want a complaint naming the header", err)
	}
}

// Whichever way a ban and a request interleave, the request must not be left
// running. These are the only two orders possible around admit, and both are
// driven explicitly here rather than left to timing.
func TestAdmitSurvivesBothInterleavings(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("203.0.113.42")

	t.Run("ban lands after the request is admitted", func(t *testing.T) {
		app := newTestApp(now)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		release, allowed := app.admit(addr, cancel)
		defer release()
		if !allowed {
			t.Fatal("nothing was banned yet, the request should have been admitted")
		}

		if _, dropped := app.Ban(addr, now.Add(time.Minute)); dropped != 1 {
			t.Fatalf("dropped=%d want=1, the admitted request must be torn down", dropped)
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatal("the admitted request was not cancelled by the ban")
		}
	})

	t.Run("ban lands before the request is admitted", func(t *testing.T) {
		app := newTestApp(now)
		_, cancel := context.WithCancel(context.Background())
		defer cancel()

		if _, dropped := app.Ban(addr, now.Add(time.Minute)); dropped != 0 {
			t.Fatalf("dropped=%d want=0, nothing was in flight", dropped)
		}
		release, allowed := app.admit(addr, cancel)
		defer release()
		if allowed {
			t.Fatal("a request from a banned address must be refused")
		}
	})
}

// admit must deregister on the refused path too, or the registry grows by one
// entry for every request a banned client keeps making.
func TestAdmitReleasesOnTheRefusedPath(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Minute))

	for i := 0; i < 100; i++ {
		_, cancel := context.WithCancel(context.Background())
		release, allowed := app.admit(addr, cancel)
		if allowed {
			t.Fatal("a banned address must not be admitted")
		}
		release()
		cancel()
	}

	app.active.mu.Lock()
	remaining := len(app.active.byAddr)
	app.active.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("registry leaked %d entries", remaining)
	}
}

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

func newBackground() context.Context { return context.Background() }

// The registration must happen before the blocklist check, not after. With the
// order reversed, a ban landing in the gap finds nothing to cancel and the
// request runs on forever; this drives exactly that moment.
func TestAdmitRegistersBeforeItChecks(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dropped := -1
	admitTestHook = func() { _, dropped = app.Ban(addr, now.Add(time.Minute)) }
	defer func() { admitTestHook = nil }()

	release, allowed := app.admit(addr, cancel)
	defer release()

	if dropped != 1 {
		t.Fatalf("the ban saw %d in-flight requests, want 1: the request was not registered before the check", dropped)
	}
	if allowed {
		t.Fatal("a request must not be admitted once the ban is stored")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the request was not cancelled by the ban that landed in the gap")
	}
}
