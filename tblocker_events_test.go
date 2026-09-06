package tblocker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func provisionedApp(t *testing.T, app *App) *App {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := app.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

// Caddy provisions, starts and stops apps around every config load. Stop must
// cope with never having been started, with being called twice, and must not
// leave the sweeper goroutine behind.
func TestAppLifecycle(t *testing.T) {
	t.Run("stop without start", func(t *testing.T) {
		app := provisionedApp(t, &App{})
		if err := app.Stop(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stop twice", func(t *testing.T) {
		app := provisionedApp(t, &App{SweepInterval: caddy.Duration(time.Millisecond)})
		if err := app.Start(); err != nil {
			t.Fatal(err)
		}
		if err := app.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := app.Stop(); err != nil {
			t.Fatalf("a second Stop must be harmless: %v", err)
		}
	})

	t.Run("no goroutine is left behind", func(t *testing.T) {
		settle := func() int {
			for i := 0; i < 50; i++ {
				runtime.Gosched()
				time.Sleep(2 * time.Millisecond)
			}
			return runtime.NumGoroutine()
		}
		before := settle()
		for i := 0; i < 20; i++ {
			app := provisionedApp(t, &App{SweepInterval: caddy.Duration(time.Millisecond)})
			if err := app.Start(); err != nil {
				t.Fatal(err)
			}
			if err := app.Stop(); err != nil {
				t.Fatal(err)
			}
		}
		if after := settle(); after > before+2 {
			t.Fatalf("goroutines before=%d after=%d, the sweeper is leaking", before, after)
		}
	})
}

// An expiry that is not in the future must never produce a live ban, whichever
// way it reaches the store.
func TestBanWithANonFutureExpiryIsNeverLive(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("203.0.113.42")

	for _, expiresAt := range []time.Time{now, now.Add(-time.Hour)} {
		// Nothing about it is live, and looking it up reclaims it on the spot.
		app := newTestApp(now)
		if stored, _ := app.Ban(addr, expiresAt); !stored {
			t.Fatal("Ban should still accept the write; liveness is decided on read")
		}
		if entries := app.Bans(); len(entries) != 0 {
			t.Fatalf("the listing must not show it: %+v", entries)
		}
		if app.IsBanned(addr) {
			t.Fatalf("expiry %s must not block anything", expiresAt)
		}
		app.mu.RLock()
		remaining := len(app.bans)
		app.mu.RUnlock()
		if remaining != 0 {
			t.Fatalf("the lookup should have reclaimed the row, %d left", remaining)
		}

		// And with nobody ever looking it up, the sweeper reclaims it instead.
		unread := newTestApp(now)
		unread.Ban(addr, expiresAt)
		if removed := unread.Sweep(); removed != 1 {
			t.Fatalf("the sweeper should reclaim it, removed=%d", removed)
		}
	}
}

// Two neighbours share one entry once a prefix widens the ban. A shorter ban
// for the second must not cut the first one short.
func TestNeighbourBanDoesNotShortenTheSharedEntry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.v4Bits = 24

	app.Ban(netip.MustParseAddr("203.0.113.10"), now.Add(time.Hour))
	app.Ban(netip.MustParseAddr("203.0.113.20"), now.Add(time.Minute))

	entries := app.Bans()
	if len(entries) != 1 {
		t.Fatalf("the two addresses must share one entry: %+v", entries)
	}
	if !entries[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expiresAt=%s, the longer ban must win", entries[0].ExpiresAt)
	}
	for _, ip := range []string{"203.0.113.10", "203.0.113.20", "203.0.113.254"} {
		if !app.IsBanned(netip.MustParseAddr(ip)) {
			t.Fatalf("%s is inside the banned /24 and must be blocked", ip)
		}
	}
	if app.IsBanned(netip.MustParseAddr("203.0.114.10")) {
		t.Fatal("a neighbouring /24 must not be affected")
	}
}

// Duplicate reports for one address arrive concurrently in practice; the
// longest expiry has to survive regardless of the order they land in.
func TestConcurrentDuplicateBansKeepTheLongest(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("203.0.113.42")

	for attempt := 0; attempt < 200; attempt++ {
		app := newTestApp(now)
		done := make(chan struct{})
		for _, minutes := range []int{1, 30, 5, 60, 10} {
			go func() {
				app.Ban(addr, now.Add(time.Duration(minutes)*time.Minute))
				done <- struct{}{}
			}()
		}
		for i := 0; i < 5; i++ {
			<-done
		}
		entries := app.Bans()
		if len(entries) != 1 || !entries[0].ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("attempt %d: entries=%+v, want a single 60m ban", attempt, entries)
		}
	}
}

// Caddy hands the handler whatever string it resolved. An IPv4-mapped form has
// to match a ban stored for the plain address, or a client could dodge the ban
// by arriving over a dual-stack listener.
func TestHandlerMatchesMappedClientIPString(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Minute))
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	if _, passed := serve(t, handler, requestWithClientIP("::ffff:203.0.113.42")); passed {
		t.Fatal("the mapped form of a banned address must be blocked")
	}
}

// A client that hung up before the handler ran leaves an already-cancelled
// context. Tracking it must not misbehave, and the request must still be
// forwarded so the rest of the chain can unwind normally.
func TestAdmitHandlesAnAlreadyCancelledRequest(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

	request := requestWithClientIP("203.0.113.42")
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)

	recorder := httptest.NewRecorder()
	reached := false
	err := handler.ServeHTTP(recorder, request, caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		reached = true
		if r.Context().Err() == nil {
			t.Error("the cancellation of the parent context must propagate")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("the request should still have been forwarded")
	}

	app.active.mu.Lock()
	remaining := len(app.active.byAddr)
	app.active.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("registry leaked %d entries", remaining)
	}
}

// Without drop_existing nothing is registered, so a ban reports no teardown
// even while requests are in flight.
func TestWithoutDropExistingBanReportsNoTeardown(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		recorder := httptest.NewRecorder()
		_ = handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
			caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
				close(entered)
				<-release
				return nil
			}))
	}()

	<-entered
	if _, dropped := app.Ban(addr, now.Add(time.Minute)); dropped != 0 {
		t.Fatalf("dropped=%d want=0 when drop_existing is off", dropped)
	}
	close(release)
	<-done
}

// An ignored address is admitted and must never be torn down, even when a
// report for it arrives.
func TestIgnoredAddressIsAdmittedAndNeverCancelled(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.ignore = []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")}
	addr := netip.MustParseAddr("192.168.243.2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, allowed := app.admit(addr, cancel)
	defer release()
	if !allowed {
		t.Fatal("an ignored address must always be admitted")
	}

	if stored, dropped := app.Ban(addr, now.Add(time.Minute)); stored || dropped != 0 {
		t.Fatalf("stored=%v dropped=%d, an ignored address must be left alone", stored, dropped)
	}
	select {
	case <-ctx.Done():
		t.Fatal("an ignored address must never be cancelled")
	default:
	}
}

// Once the ban expires the next request is admitted again without anyone
// having to release it by hand.
func TestRequestIsAdmittedAgainAfterTheBanExpires(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Minute))

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if release, allowed := app.admit(addr, cancel); allowed {
		release()
		t.Fatal("still inside the ban window")
	} else {
		release()
	}

	app.now = func() time.Time { return now.Add(2 * time.Minute) }
	release, allowed := app.admit(addr, cancel)
	defer release()
	if !allowed {
		t.Fatal("the ban has expired, the request must be admitted")
	}
}

// Two sites share one store. A report arriving on one of them has to tear down
// what the other one is serving for the same address.
func TestBanCrossesBetweenSitesSharingTheApp(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")

	subscription := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}
	tunnel := Handler{StatusCode: http.StatusUnauthorized, DropExisting: true, app: app}

	entered := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	for _, handler := range []Handler{subscription, tunnel} {
		go func() {
			recorder := httptest.NewRecorder()
			_ = handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
				caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
					entered <- struct{}{}
					select {
					case <-r.Context().Done():
						cancelled <- struct{}{}
					case <-time.After(3 * time.Second):
					}
					return nil
				}))
		}()
	}
	<-entered
	<-entered

	webhook := newTestWebhook(app)
	body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":60}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-cancelled:
		case <-time.After(3 * time.Second):
			t.Fatal("a request on one of the two sites was not torn down")
		}
	}

	// Each site keeps its own status code on the refused path.
	if recorder, _ := serve(t, subscription, requestWithClientIP(addr.String())); recorder.Code != http.StatusForbidden {
		t.Fatalf("subscription status=%d", recorder.Code)
	}
	if recorder, _ := serve(t, tunnel, requestWithClientIP(addr.String())); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tunnel status=%d", recorder.Code)
	}
}

// Wrong methods must be refused with an Allow header naming what is accepted.
func TestRejectedMethodsAdvertiseWhatIsAllowed(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	webhook := newTestWebhook(newTestApp(now))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		request := httptest.NewRequest(method, "http://caddy:9080/hook", nil)
		request.RemoteAddr = "192.168.243.3:40000"
		recorder := httptest.NewRecorder()
		if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d", method, recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != http.MethodPost {
			t.Fatalf("%s Allow=%q want %q", method, got, http.MethodPost)
		}
	}

	admin := newTestAdmin(newTestApp(now))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodOptions} {
		recorder := callAdmin(t, admin, method, "http://caddy:9080/admin", "192.168.243.1:40000")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d", method, recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != "GET, DELETE" {
			t.Fatalf("%s Allow=%q", method, got)
		}
	}
}

// The source guard has to work for IPv6 peers too, zone and all.
func TestSourceGuardAcceptsIPv6Peers(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}

	if !sourcePermitted("[2001:db8::1]:40000", allowed) {
		t.Fatal("an IPv6 peer inside the range must be permitted")
	}
	if !sourcePermitted("[2001:db8::1%eth0]:40000", allowed) {
		t.Fatal("a zone must not stop the match")
	}
	if sourcePermitted("[2001:db9::1]:40000", allowed) {
		t.Fatal("an IPv6 peer outside the range must be refused")
	}
	if sourcePermitted("192.0.2.1:40000", allowed) {
		t.Fatal("an IPv4 peer must not match an IPv6 range")
	}
}

// The listing is sorted so repeated polls of the admin route are stable.
func TestBansListingIsSortedAndStable(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	for i := 0; i < 40; i++ {
		app.Ban(netip.AddrFrom4([4]byte{203, 0, 113, byte(i)}), now.Add(time.Minute))
	}

	first := app.Bans()
	if len(first) != 40 {
		t.Fatalf("entries=%d", len(first))
	}
	if !sort.SliceIsSorted(first, func(i, j int) bool { return first[i].IP < first[j].IP }) {
		t.Fatal("the listing is not sorted")
	}
	for attempt := 0; attempt < 20; attempt++ {
		again := app.Bans()
		for i := range again {
			if again[i].IP != first[i].IP {
				t.Fatalf("listing changed order between calls at %d", i)
			}
		}
	}
}
