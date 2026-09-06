package tblocker

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// A body larger than max_body must be refused rather than read into memory.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)
	webhook.MaxBody = 256

	padding := strings.Repeat("a", 4096)
	body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":60},"pad":"` + padding + `"}`

	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400", recorder.Code)
	}
	if entries := app.Bans(); len(entries) != 0 {
		t.Fatalf("an oversized body must not produce a ban: %+v", entries)
	}
}

// A body just under the limit must still be accepted.
func TestWebhookAcceptsBodyUnderTheLimit(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)
	webhook.MaxBody = 4096

	body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":60},"pad":"` +
		strings.Repeat("a", 1024) + `"}`

	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d want=204", recorder.Code)
	}
	if entries := app.Bans(); len(entries) != 1 {
		t.Fatalf("entries=%+v", entries)
	}
}

// An absolute expiry far in the future is capped by max_ttl, the same as a
// relative one.
func TestWebhookClampsFarFutureWillUnblockAt(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)

	body := `{"actionReport":{"ip":"203.0.113.42","willUnblockAt":"9999-12-31T23:59:59Z"}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}
	entries := app.Bans()
	if len(entries) != 1 || !entries[0].ExpiresAt.Equal(now.Add(time.Duration(app.MaxTTL))) {
		t.Fatalf("entries=%+v", entries)
	}
}

// Reports carrying malformed or unusable addresses must never reach the store.
func TestWebhookRejectsUnusableAddresses(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for _, ip := range []string{
		"203.0.113.42:1234", // host:port, not an address
		"not-an-ip",
		"0.0.0.0",
		"::",
		"", // absent
		"999.1.1.1",
		"203.0.113.42/32", // a prefix, not an address
	} {
		app := newTestApp(now)
		webhook := newTestWebhook(app)
		body, err := json.Marshal(map[string]any{
			"actionReport": map[string]any{"ip": ip, "blockDuration": 60},
		})
		if err != nil {
			t.Fatal(err)
		}
		if recorder := postReport(t, webhook, string(body), "192.168.243.3:40000"); recorder.Code != http.StatusBadRequest {
			t.Fatalf("ip=%q status=%d want=400", ip, recorder.Code)
		}
		if entries := app.Bans(); len(entries) != 0 {
			t.Fatalf("ip=%q produced a ban: %+v", ip, entries)
		}
	}
}

// An address carrying an IPv6 zone must resolve to the same entry as the
// zoneless form, otherwise a ban could be dodged by attaching a zone.
func TestZoneIsStrippedOnBothSides(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)

	app.Ban(netip.MustParseAddr("fe80::1%eth0"), now.Add(time.Minute))
	if !app.IsBanned(netip.MustParseAddr("fe80::1")) {
		t.Fatal("a zoned ban must match the zoneless address")
	}
	if !app.IsBanned(netip.MustParseAddr("fe80::1%eth1")) {
		t.Fatal("a ban must match regardless of zone")
	}
}

// An ignore entry has to win over a ban that is already stored, so adding an
// address to the list takes effect without a restart of the ban's TTL.
func TestIgnoreWinsOverAnExistingEntry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Minute))
	if !app.IsBanned(addr) {
		t.Fatal("precondition: the address should be banned")
	}

	app.ignore = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	if app.IsBanned(addr) {
		t.Fatal("an ignored address must never be reported as banned")
	}
}

// Provision runs on every config load; the derived ignore list must not
// accumulate across calls.
func TestProvisionDoesNotAccumulateIgnoreEntries(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: newBackground()})
	defer cancel()

	app := &App{Ignore: []string{"192.168.243.0/28", "127.0.0.0/8"}}
	for i := 0; i < 3; i++ {
		if err := app.Provision(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(app.ignore) != 2 {
		t.Fatalf("ignore=%d entries after three provisions, want 2", len(app.ignore))
	}
}

// Flush and Unban must also clear entries that are already expired, or the
// store keeps rows the admin route reports as gone.
func TestUnbanAndFlushRemoveExpiredRows(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Second))
	app.now = func() time.Time { return now.Add(time.Hour) }

	if app.Unban(addr) {
		t.Fatal("Unban must report false for an entry that already expired")
	}
	app.mu.RLock()
	remaining := len(app.bans)
	app.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("Unban left %d rows behind", remaining)
	}

	app.now = func() time.Time { return now }
	app.Ban(addr, now.Add(time.Second))
	app.now = func() time.Time { return now.Add(time.Hour) }
	if removed := app.Flush(); removed != 0 {
		t.Fatalf("Flush reported %d live removals for an expired row", removed)
	}
	app.mu.RLock()
	remaining = len(app.bans)
	app.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("Flush left %d rows behind", remaining)
	}
}

// The admin listing has to round-trip: whatever it prints must be accepted
// back by the release endpoint, including when bans are widened to a prefix.
func TestAdminListingRoundTripsThroughRelease(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.v4Bits = 24
	app.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Minute))
	admin := newTestAdmin(app)

	entries := app.Bans()
	if len(entries) != 1 {
		t.Fatalf("entries=%+v", entries)
	}
	recorder := callAdmin(t, admin, http.MethodDelete,
		"http://caddy:9080/admin?ip="+entries[0].IP, "192.168.243.1:40000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s ip=%s", recorder.Code, recorder.Body.String(), entries[0].IP)
	}
	if app.IsBanned(netip.MustParseAddr("203.0.113.42")) {
		t.Fatal("the release did not take effect")
	}
}
