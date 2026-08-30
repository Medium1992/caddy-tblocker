package tblocker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func newTestApp(now time.Time) *App {
	return &App{
		DefaultTTL: caddy.Duration(time.Minute),
		MaxTTL:     caddy.Duration(time.Hour),
		MaxEntries: defaultMaxEntries,
		v4Bits:     32,
		v6Bits:     128,
		bans:       make(map[netip.Addr]time.Time),
		now:        func() time.Time { return now },
		active:     activeRequests{byAddr: make(map[netip.Addr]map[uint64]context.CancelFunc)},
	}
}

func requestWithClientIP(ip string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	request = request.WithContext(context.WithValue(request.Context(), caddyhttp.VarsCtxKey, map[string]any{}))
	caddyhttp.SetVar(request.Context(), caddyhttp.ClientIPVarKey, ip)
	return request
}

// serve runs the handler and reports whether the request reached the next one.
func serve(t *testing.T, handler Handler, request *http.Request) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	passed := false
	err := handler.ServeHTTP(recorder, request, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		passed = true
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return recorder, passed
}

func TestHandlerBlocksOnlyLiveEntry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Minute))
	handler := Handler{StatusCode: http.StatusForbidden, app: app}
	request := requestWithClientIP(addr.String())

	recorder, passed := serve(t, handler, request)
	if passed || recorder.Code != http.StatusForbidden {
		t.Fatalf("passed=%v status=%d", passed, recorder.Code)
	}

	app.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, passed := serve(t, handler, request); !passed {
		t.Fatal("expired entry still blocked the request")
	}
}

func TestHandlerPassesWhenClientIPIsUnusable(t *testing.T) {
	app := newTestApp(time.Now())
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	for _, clientIP := range []string{"", "@", "not-an-ip"} {
		if _, passed := serve(t, handler, requestWithClientIP(clientIP)); !passed {
			t.Fatalf("client_ip %q must fail open", clientIP)
		}
	}
}

func TestHandlerMatchesIPv4MappedAddress(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.Ban(netip.MustParseAddr("::ffff:203.0.113.42"), now.Add(time.Minute))
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	if _, passed := serve(t, handler, requestWithClientIP("203.0.113.42")); passed {
		t.Fatal("mapped and plain forms of the same address must share one entry")
	}
}

func TestIgnoredAddressIsNeverBanned(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.ignore = []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")}
	addr := netip.MustParseAddr("192.168.243.2")

	if stored, _ := app.Ban(addr, now.Add(time.Hour)); stored {
		t.Fatal("Ban must refuse a protected address")
	}
	if app.IsBanned(addr) {
		t.Fatal("a protected address must never be reported as banned")
	}
}

func TestBanPrefixWidensTheEntry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.v6Bits = 64
	app.Ban(netip.MustParseAddr("2001:db8:1:2::dead"), now.Add(time.Minute))

	if !app.IsBanned(netip.MustParseAddr("2001:db8:1:2::beef")) {
		t.Fatal("another address inside the banned /64 must be blocked")
	}
	if app.IsBanned(netip.MustParseAddr("2001:db8:1:3::beef")) {
		t.Fatal("an address outside the banned /64 must not be blocked")
	}
	// IPv4 keeps its own width and stays a single host by default.
	app.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Minute))
	if app.IsBanned(netip.MustParseAddr("203.0.113.43")) {
		t.Fatal("ipv4 bans must not widen when only ipv6_prefix is set")
	}
}

func TestBanKeepsTheLaterExpiration(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")

	app.Ban(addr, now.Add(time.Hour))
	app.Ban(addr, now.Add(time.Minute))

	entries := app.Bans()
	if len(entries) != 1 || !entries[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("a shorter duplicate report must not shorten the ban: %+v", entries)
	}
}

func TestMaxEntriesBoundsTheStore(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.MaxEntries = 2

	if stored, _ := app.Ban(netip.MustParseAddr("203.0.113.1"), now.Add(time.Minute)); !stored {
		t.Fatal("first ban must be stored")
	}
	// An entry that is already expired must be reclaimed before refusing a ban.
	app.Ban(netip.MustParseAddr("203.0.113.2"), now.Add(time.Second))
	app.now = func() time.Time { return now.Add(time.Minute / 2) }

	if stored, _ := app.Ban(netip.MustParseAddr("203.0.113.3"), now.Add(time.Minute)); !stored {
		t.Fatal("an expired entry must be reclaimed to make room")
	}
	if stored, _ := app.Ban(netip.MustParseAddr("203.0.113.4"), now.Add(time.Minute)); stored {
		t.Fatal("a full store must refuse further bans")
	}
}

func TestSweepDropsExpiredEntriesWithoutLookup(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.Ban(netip.MustParseAddr("203.0.113.1"), now.Add(time.Second))
	app.Ban(netip.MustParseAddr("203.0.113.2"), now.Add(time.Hour))

	app.now = func() time.Time { return now.Add(time.Minute) }
	if removed := app.Sweep(); removed != 1 {
		t.Fatalf("removed=%d want=1", removed)
	}
	app.mu.RLock()
	remaining := len(app.bans)
	app.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("remaining=%d want=1", remaining)
	}
}

func newTestWebhook(app *App) Webhook {
	return Webhook{
		Allow:   []string{"192.168.243.0/28"},
		allowed: []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")},
		MaxBody: 4096,
		app:     app,
	}
}

func postReport(t *testing.T, webhook Webhook, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://caddy:9080/internal/tblocker/token", bytes.NewBufferString(body))
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestWebhookAcceptsNodeReportAndClampsTTL(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)

	body := `{"actionReport":{"blocked":true,"ip":"203.0.113.42","blockDuration":86400,` +
		`"willUnblockAt":"2026-08-29T12:00:00.000Z"},"xrayReport":{"source":"203.0.113.42:0","email":"user@example.test"}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	entries := app.Bans()
	if len(entries) != 1 || entries[0].IP != "203.0.113.42" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if want := now.Add(time.Hour); !entries[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt=%s want=%s (max_ttl must cap the report)", entries[0].ExpiresAt, want)
	}
}

// The node stamps willUnblockAt with its own clock. A node running behind Caddy
// must not silently void the ban, so blockDuration wins whenever it is present.
func TestWebhookPrefersBlockDurationOverSkewedTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)

	body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":600,"willUnblockAt":"2026-08-28T11:30:00.000Z"}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}

	entries := app.Bans()
	if len(entries) != 1 {
		t.Fatalf("a report with a past willUnblockAt must still ban: %+v", entries)
	}
	if want := now.Add(10 * time.Minute); !entries[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt=%s want=%s", entries[0].ExpiresAt, want)
	}
}

func TestWebhookFallsBackToDefaultTTL(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := newTestWebhook(app)

	// blockDuration 0 means "permanent" to the node's nftables service, which
	// an in-memory store cannot express; default_ttl applies instead.
	body := `{"actionReport":{"ip":"203.0.113.42","blockDuration":0}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}

	entries := app.Bans()
	if len(entries) != 1 || !entries[0].ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestWebhookRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		body       string
		remoteAddr string
		method     string
		want       int
	}{
		{name: "untrusted source", body: `{}`, remoteAddr: "198.51.100.10:40000", want: http.StatusForbidden},
		{name: "malformed json", body: `{"actionReport":`, remoteAddr: "192.168.243.3:40000", want: http.StatusBadRequest},
		{name: "trailing json", body: `{"actionReport":{"ip":"203.0.113.1"}}{"a":1}`, remoteAddr: "192.168.243.3:40000", want: http.StatusBadRequest},
		{name: "missing ip", body: `{"actionReport":{"blockDuration":60}}`, remoteAddr: "192.168.243.3:40000", want: http.StatusBadRequest},
		{name: "unspecified ip", body: `{"actionReport":{"ip":"0.0.0.0"}}`, remoteAddr: "192.168.243.3:40000", want: http.StatusBadRequest},
		{name: "wrong method", method: http.MethodGet, remoteAddr: "192.168.243.3:40000", want: http.StatusMethodNotAllowed},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			webhook := newTestWebhook(newTestApp(now))
			method := testCase.method
			if method == "" {
				method = http.MethodPost
			}
			request := httptest.NewRequest(method, "http://caddy:9080/internal/tblocker/token", bytes.NewBufferString(testCase.body))
			request.RemoteAddr = testCase.remoteAddr
			recorder := httptest.NewRecorder()
			if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != testCase.want {
				t.Fatalf("status=%d want=%d", recorder.Code, testCase.want)
			}
		})
	}
}

// A report is acknowledged even when the address is protected, so the node does
// not retry, but nothing is stored.
func TestWebhookAcknowledgesIgnoredAddressWithoutStoringIt(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.ignore = []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")}
	webhook := newTestWebhook(app)

	body := `{"actionReport":{"ip":"192.168.243.2","blockDuration":3600}}`
	if recorder := postReport(t, webhook, body, "192.168.243.3:40000"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}
	if entries := app.Bans(); len(entries) != 0 {
		t.Fatalf("a protected address must not be stored: %+v", entries)
	}
}

func newTestAdmin(app *App) Admin {
	return Admin{
		Allow:   []string{"192.168.243.0/28"},
		allowed: []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")},
		app:     app,
	}
}

func callAdmin(t *testing.T, admin Admin, method, target, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	if err := admin.ServeHTTP(recorder, request, nil); err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestAdminListsAndReleasesBans(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Hour))
	app.Ban(netip.MustParseAddr("203.0.113.7"), now.Add(time.Hour))
	admin := newTestAdmin(app)

	recorder := callAdmin(t, admin, http.MethodGet, "http://caddy:9080/admin", "192.168.243.1:40000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var listing struct {
		Count int        `json:"count"`
		Bans  []BanEntry `json:"bans"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Count != 2 || listing.Bans[0].IP != "203.0.113.42" {
		// Sorted lexically, so .42 precedes .7.
		t.Fatalf("unexpected listing: %+v", listing)
	}

	recorder = callAdmin(t, admin, http.MethodDelete, "http://caddy:9080/admin?ip=203.0.113.42", "192.168.243.1:40000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if app.IsBanned(netip.MustParseAddr("203.0.113.42")) {
		t.Fatal("the released address is still banned")
	}
	if !app.IsBanned(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("releasing one address must not touch the others")
	}

	if recorder := callAdmin(t, admin, http.MethodDelete, "http://caddy:9080/admin?ip=203.0.113.42", "192.168.243.1:40000"); recorder.Code != http.StatusNotFound {
		t.Fatalf("releasing an unknown address: status=%d want=404", recorder.Code)
	}

	recorder = callAdmin(t, admin, http.MethodDelete, "http://caddy:9080/admin", "192.168.243.1:40000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if entries := app.Bans(); len(entries) != 0 {
		t.Fatalf("flush left entries behind: %+v", entries)
	}
}

func TestAdminRejectsUntrustedSourceAndBadInput(t *testing.T) {
	app := newTestApp(time.Now())
	admin := newTestAdmin(app)

	if recorder := callAdmin(t, admin, http.MethodGet, "http://caddy:9080/admin", "198.51.100.10:40000"); recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=403", recorder.Code)
	}
	if recorder := callAdmin(t, admin, http.MethodDelete, "http://caddy:9080/admin?ip=nope", "192.168.243.1:40000"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400", recorder.Code)
	}
	if recorder := callAdmin(t, admin, http.MethodPost, "http://caddy:9080/admin", "192.168.243.1:40000"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=405", recorder.Code)
	}
}

func TestSourcePermittedIgnoresForwardingHeaders(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")}

	if !sourcePermitted("192.168.243.3:40000", allowed) {
		t.Fatal("a peer inside the allow range must be permitted")
	}
	if sourcePermitted("198.51.100.10:40000", allowed) {
		t.Fatal("a peer outside the allow range must be refused")
	}
	if sourcePermitted("not-an-address", allowed) {
		t.Fatal("an unparsable peer must be refused")
	}
	if !sourcePermitted("[::ffff:192.168.243.3]:40000", allowed) {
		t.Fatal("an IPv4-mapped peer must match its IPv4 range")
	}
}

func TestValidateStatus(t *testing.T) {
	for _, status := range []int{400, 403, 429, 503, 599} {
		if err := validateStatus(status); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
	}
	for _, status := range []int{0, 200, 302, 399, 600} {
		if err := validateStatus(status); err == nil {
			t.Fatalf("status %d must be rejected", status)
		}
	}
}

func TestProvisionValidatesConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		build   func() *App
		wantErr bool
	}{
		{name: "defaults", build: func() *App { return &App{} }},
		{name: "ttl inverted", build: func() *App {
			return &App{DefaultTTL: caddy.Duration(time.Hour), MaxTTL: caddy.Duration(time.Second)}
		}, wantErr: true},
		{name: "negative sweep", build: func() *App { return &App{SweepInterval: caddy.Duration(-time.Second)} }, wantErr: true},
		{name: "ipv4 prefix too wide", build: func() *App { return &App{IPv4Prefix: 33} }, wantErr: true},
		{name: "ipv6 prefix too wide", build: func() *App { return &App{IPv6Prefix: 129} }, wantErr: true},
		{name: "bad ignore cidr", build: func() *App { return &App{Ignore: []string{"192.168.243.2"}} }, wantErr: true},
		{name: "good ignore cidr", build: func() *App { return &App{Ignore: []string{"192.168.243.0/28"}} }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
			defer cancel()
			err := testCase.build().Provision(ctx)
			if testCase.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, testCase.wantErr)
			}
		})
	}
}

func TestSweeperStartsAndStops(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	app := &App{SweepInterval: caddy.Duration(time.Millisecond)}
	if err := app.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	app.Ban(netip.MustParseAddr("203.0.113.42"), time.Now().Add(time.Millisecond))
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		app.mu.RLock()
		remaining := len(app.bans)
		app.mu.RUnlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweeper did not purge the expired entry")
		}
		time.Sleep(time.Millisecond)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}

// A ban must tear down the tunnels that are already running for that address.
// Caddy's reverse proxy closes the upstream connection when the request
// context is done, so cancelling it is what ends a live WebSocket or XHTTP
// stream instead of letting it run to completion.
func TestDropExistingCancelsInFlightRequests(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		recorder := httptest.NewRecorder()
		finished <- handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
			caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
				close(started)
				<-r.Context().Done() // stands in for a live proxied tunnel
				return r.Context().Err()
			}))
	}()

	<-started
	_, dropped := app.Ban(addr, now.Add(time.Minute))
	if dropped != 1 {
		t.Fatalf("dropped=%d want=1", dropped)
	}

	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("the in-flight request should have been cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request was never cancelled")
	}

	// The registry must not leak once the handler returns.
	app.active.mu.Lock()
	remaining := len(app.active.byAddr)
	app.active.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("registry leaked %d entries", remaining)
	}
}

// Without the option the module keeps its previous behaviour: an established
// tunnel is left alone and only the next request is refused.
func TestWithoutDropExistingNothingIsTracked(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		recorder := httptest.NewRecorder()
		finished <- handler.ServeHTTP(recorder, requestWithClientIP(addr.String()),
			caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
				close(started)
				<-release
				return nil
			}))
	}()

	<-started
	if _, dropped := app.Ban(addr, now.Add(time.Minute)); dropped != 0 {
		t.Fatalf("dropped=%d want=0", dropped)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

// Requests from other addresses must survive a ban.
func TestDropExistingLeavesOtherClientsAlone(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	victim := netip.MustParseAddr("203.0.113.42")
	bystander := netip.MustParseAddr("203.0.113.43")
	handler := Handler{StatusCode: http.StatusForbidden, DropExisting: true, app: app}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		recorder := httptest.NewRecorder()
		finished <- handler.ServeHTTP(recorder, requestWithClientIP(bystander.String()),
			caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-r.Context().Done():
					return r.Context().Err()
				}
			}))
	}()

	<-started
	if _, dropped := app.Ban(victim, now.Add(time.Minute)); dropped != 0 {
		t.Fatalf("dropped=%d want=0, a ban must not touch another address", dropped)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("the bystander was cancelled: %v", err)
	}
}
