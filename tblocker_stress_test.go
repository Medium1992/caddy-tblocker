package tblocker

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Every exported operation runs against one store at once. The point is not a
// specific assertion but that nothing deadlocks, panics, or trips the race
// detector: Ban takes the store lock then the registry lock, admit takes them
// in the opposite order, and the sweeper walks the map underneath both.
func TestConcurrentOperationsDoNotDeadlock(t *testing.T) {
	app := &App{
		DefaultTTL:    caddy.Duration(time.Minute),
		MaxTTL:        caddy.Duration(time.Hour),
		MaxEntries:    512,
		SweepInterval: caddy.Duration(time.Millisecond),
	}
	ctx, cancelCtx := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancelCtx()
	if err := app.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Stop(); err != nil {
			t.Fatal(err)
		}
	}()

	addrs := make([]netip.Addr, 32)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{203, 0, 113, byte(i)})
	}

	deadline := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	var admitted, refused atomic.Int64

	worker := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				fn(i)
			}
		}()
	}

	for w := 0; w < 4; w++ {
		worker(func(i int) { app.Ban(addrs[i%len(addrs)], time.Now().Add(time.Duration(i%50)*time.Millisecond)) })
		worker(func(i int) { app.IsBanned(addrs[i%len(addrs)]) })
		worker(func(i int) {
			_, cancel := context.WithCancel(context.Background())
			release, ok := app.admit(addrs[i%len(addrs)], cancel)
			if ok {
				admitted.Add(1)
			} else {
				refused.Add(1)
			}
			release()
			cancel()
		})
		worker(func(i int) { app.Unban(addrs[i%len(addrs)]) })
		worker(func(i int) { app.Bans() })
		worker(func(i int) { app.Sweep() })
	}
	worker(func(int) { app.Flush() })

	wg.Wait()
	if admitted.Load()+refused.Load() == 0 {
		t.Fatal("the admit workers never ran")
	}
	t.Logf("admitted=%d refused=%d", admitted.Load(), refused.Load())
}

// Long-lived requests that only ever end when their context is cancelled,
// racing against a ban for the same address. Every request that gets past the
// handler must be torn down by that ban; a request that is still running when
// the ban has been in place for seconds is exactly the failure this whole
// mechanism exists to prevent.
func TestHandlerUnderConcurrentBans(t *testing.T) {
	const clients = 16
	const perClient = 25

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

	var wg sync.WaitGroup
	var admitted, cancelled, stuck atomic.Int64

	for c := 0; c < clients; c++ {
		addr := netip.AddrFrom4([4]byte{203, 0, 113, byte(c)})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				recorder := httptest.NewRecorder()
				_ = handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
					caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
						admitted.Add(1)
						select {
						case <-r.Context().Done():
							cancelled.Add(1)
						case <-time.After(5 * time.Second):
							stuck.Add(1)
						}
						return nil
					}))
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				app.Ban(addr, now.Add(time.Minute))
				time.Sleep(time.Millisecond)
				app.Unban(addr)
			}
			// Leave the address banned so nothing can linger at the end.
			app.Ban(addr, now.Add(time.Minute))
		}()
	}

	wg.Wait()
	if stuck.Load() != 0 {
		t.Fatalf("%d admitted requests were never cancelled by their ban", stuck.Load())
	}
	if admitted.Load() == 0 {
		t.Fatal("no request was ever admitted, the test proved nothing")
	}
	if cancelled.Load() != admitted.Load() {
		t.Fatalf("admitted=%d cancelled=%d, every admitted request must be torn down",
			admitted.Load(), cancelled.Load())
	}
}

// Garbage on the webhook must never panic the handler, whatever it contains.
func FuzzWebhookReport(f *testing.F) {
	f.Add(`{"actionReport":{"ip":"203.0.113.42","blockDuration":60}}`)
	f.Add(`{"actionReport":{"ip":"::1","willUnblockAt":"2026-08-28T13:00:00Z"}}`)
	f.Add(`{"actionReport":{"ip":"203.0.113.42","blockDuration":-9223372036854775808}}`)
	f.Add(`{"actionReport":{"ip":"","blockDuration":1e999}}`)
	f.Add(`{"actionReport":null}`)
	f.Add(`[]`)
	f.Add(``)
	f.Add(`{"actionReport":{"ip":"203.0.113.42","willUnblockAt":"not-a-date"}}`)
	f.Add("{\"actionReport\":{\"ip\":\"203.0.113.42\"}}\x00")

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, body string) {
		app := newTestApp(now)
		webhook := newTestWebhook(app)

		request := httptest.NewRequest(http.MethodPost, "http://caddy:9080/hook", bytes.NewBufferString(body))
		request.RemoteAddr = "192.168.243.3:40000"
		recorder := httptest.NewRecorder()
		if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
			t.Fatalf("handler returned an error: %v", err)
		}

		switch recorder.Code {
		case http.StatusNoContent, http.StatusBadRequest:
		default:
			t.Fatalf("unexpected status %d for %q", recorder.Code, body)
		}

		// Anything that was stored must be a live, sane entry.
		for _, entry := range app.Bans() {
			addr, err := netip.ParseAddr(entry.IP)
			if err != nil {
				t.Fatalf("stored an unparsable address %q", entry.IP)
			}
			if addr.IsUnspecified() {
				t.Fatalf("stored the unspecified address from %q", body)
			}
			if !entry.ExpiresAt.After(now) {
				t.Fatalf("stored an already expired ban %s from %q", entry.ExpiresAt, body)
			}
			if entry.ExpiresAt.After(now.Add(time.Duration(app.MaxTTL))) {
				t.Fatalf("stored a ban beyond max_ttl: %s from %q", entry.ExpiresAt, body)
			}
		}
	})
}
