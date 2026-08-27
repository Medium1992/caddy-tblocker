package tblocker

import (
	"bytes"
	"context"
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
		bans:       make(map[netip.Addr]time.Time),
		now:        func() time.Time { return now },
	}
}

func TestHandlerBlocksOnlyLiveEntry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	addr := netip.MustParseAddr("203.0.113.42")
	app.Ban(addr, now.Add(time.Minute))
	handler := Handler{StatusCode: http.StatusForbidden, app: app}

	request := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	request = request.WithContext(context.WithValue(request.Context(), caddyhttp.VarsCtxKey, map[string]any{}))
	caddyhttp.SetVar(request.Context(), caddyhttp.ClientIPVarKey, addr.String())
	recorder := httptest.NewRecorder()
	called := false
	err := handler.ServeHTTP(recorder, request, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		called = true
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if called || recorder.Code != http.StatusForbidden {
		t.Fatalf("called=%v status=%d", called, recorder.Code)
	}

	app.now = func() time.Time { return now.Add(2 * time.Minute) }
	recorder = httptest.NewRecorder()
	called = false
	if err := handler.ServeHTTP(recorder, request, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		called = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expired entry still blocked the request")
	}
}

func TestWebhookAcceptsRemnaReportAndClampsTTL(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	webhook := Webhook{
		Allow:   []string{"192.168.243.0/28"},
		allowed: []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")},
		MaxBody: 4096,
		app:     app,
	}
	body := []byte(`{"actionReport":{"ip":"203.0.113.42","blockDuration":3600,"willUnblockAt":"2026-08-30T12:00:00Z"},"xrayReport":{"source":"203.0.113.42","email":"user@example.test"}}`)
	request := httptest.NewRequest(http.MethodPost, "http://caddy:9080/internal/tblocker/token", bytes.NewReader(body))
	request.RemoteAddr = "192.168.243.3:40000"
	recorder := httptest.NewRecorder()
	if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	app.mu.RLock()
	expiresAt := app.bans[netip.MustParseAddr("203.0.113.42")]
	app.mu.RUnlock()
	if want := now.Add(time.Hour); !expiresAt.Equal(want) {
		t.Fatalf("expiresAt=%s want=%s", expiresAt, want)
	}
}

func TestWebhookRejectsUntrustedSource(t *testing.T) {
	app := newTestApp(time.Now())
	webhook := Webhook{
		allowed: []netip.Prefix{netip.MustParsePrefix("192.168.243.0/28")},
		MaxBody: 4096,
		app:     app,
	}
	request := httptest.NewRequest(http.MethodPost, "http://caddy:9080/internal/tblocker/token", bytes.NewBufferString(`{}`))
	request.RemoteAddr = "198.51.100.10:40000"
	recorder := httptest.NewRecorder()
	if err := webhook.ServeHTTP(recorder, request, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d", recorder.Code)
	}
}
