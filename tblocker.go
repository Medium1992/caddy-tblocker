// Package tblocker provides a Caddy-native TTL blocklist populated by Remna
// torrent-blocker webhooks.
package tblocker

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule((*App)(nil))
	caddy.RegisterModule(Handler{})
	caddy.RegisterModule(Webhook{})

	httpcaddyfile.RegisterGlobalOption("tblocker", parseApp)
	httpcaddyfile.RegisterHandlerDirective("tblocker", parseHandler)
	httpcaddyfile.RegisterHandlerDirective("tblocker_webhook", parseWebhook)
	httpcaddyfile.RegisterDirectiveOrder("tblocker", httpcaddyfile.Before, "reverse_proxy")
	httpcaddyfile.RegisterDirectiveOrder("tblocker_webhook", httpcaddyfile.Before, "respond")
}

// App is the process-local ban store shared by HTTP handlers. Entries do not
// survive a Caddy reload, intentionally keeping the failure mode bounded.
type App struct {
	DefaultTTL caddy.Duration `json:"default_ttl,omitempty"`
	MaxTTL     caddy.Duration `json:"max_ttl,omitempty"`

	mu     sync.RWMutex
	bans   map[netip.Addr]time.Time
	logger *zap.Logger
	now    func() time.Time
}

// CaddyModule returns the module information.
func (*App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tblocker",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision initializes the in-memory store.
func (a *App) Provision(ctx caddy.Context) error {
	if a.DefaultTTL == 0 {
		a.DefaultTTL = caddy.Duration(time.Minute)
	}
	if a.MaxTTL == 0 {
		a.MaxTTL = caddy.Duration(24 * time.Hour)
	}
	if a.DefaultTTL < 0 || a.MaxTTL <= 0 || a.DefaultTTL > a.MaxTTL {
		return fmt.Errorf("tblocker: require 0 < default_ttl <= max_ttl")
	}
	a.bans = make(map[netip.Addr]time.Time)
	a.logger = ctx.Logger()
	a.now = time.Now
	return nil
}

// Start implements caddy.App.
func (*App) Start() error { return nil }

// Stop implements caddy.App.
func (*App) Stop() error { return nil }

// Ban records addr until expiresAt. A later expiration replaces an earlier one;
// a shorter duplicate report never unblocks a client prematurely.
func (a *App) Ban(addr netip.Addr, expiresAt time.Time) {
	addr = addr.Unmap()
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, ok := a.bans[addr]; !ok || expiresAt.After(current) {
		a.bans[addr] = expiresAt
	}
}

// IsBanned reports whether addr has a live ban and lazily removes expired data.
func (a *App) IsBanned(addr netip.Addr) bool {
	addr = addr.Unmap()
	now := a.clock()
	a.mu.RLock()
	expiresAt, ok := a.bans[addr]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if expiresAt.After(now) {
		return true
	}
	a.mu.Lock()
	if current, exists := a.bans[addr]; exists && !current.After(now) {
		delete(a.bans, addr)
	}
	a.mu.Unlock()
	return false
}

func (a *App) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// Handler blocks a request before it reaches the next HTTP handler when Caddy's
// trusted-proxy-aware client_ip is currently banned.
type Handler struct {
	StatusCode int `json:"status_code,omitempty"`

	app *App
}

// CaddyModule returns the module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.tblocker",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision connects the request handler to the configured app.
func (h *Handler) Provision(ctx caddy.Context) error {
	if h.StatusCode == 0 {
		h.StatusCode = http.StatusForbidden
	}
	if h.StatusCode < 400 || h.StatusCode > 599 {
		return fmt.Errorf("tblocker: status_code must be an HTTP error status")
	}
	app, err := ctx.AppIfConfigured("tblocker")
	if err != nil {
		return fmt.Errorf("tblocker: configure the global tblocker app: %w", err)
	}
	var ok bool
	h.app, ok = app.(*App)
	if !ok {
		return fmt.Errorf("tblocker: unexpected app type %T", app)
	}
	return nil
}

// ServeHTTP denies live entries without exposing ban information.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string)
	if !ok || clientIP == "" {
		return next.ServeHTTP(w, r)
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return next.ServeHTTP(w, r)
	}
	if !h.app.IsBanned(addr) {
		return next.ServeHTTP(w, r)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(h.StatusCode)
	return nil
}

// Webhook accepts a Remna torrent-blocker action report and puts its client IP
// into the shared TTL blocklist.
type Webhook struct {
	Allow   []string `json:"allow,omitempty"`
	MaxBody int64    `json:"max_body,omitempty"`

	allowed []netip.Prefix
	app     *App
}

// CaddyModule returns the module information.
func (Webhook) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.tblocker_webhook",
		New: func() caddy.Module { return new(Webhook) },
	}
}

// Provision validates the internal webhook source ranges.
func (h *Webhook) Provision(ctx caddy.Context) error {
	if h.MaxBody == 0 {
		h.MaxBody = 64 << 10
	}
	if h.MaxBody <= 0 {
		return fmt.Errorf("tblocker_webhook: max_body must be positive")
	}
	if len(h.Allow) == 0 {
		return fmt.Errorf("tblocker_webhook: at least one allow CIDR is required")
	}
	for _, raw := range h.Allow {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return fmt.Errorf("tblocker_webhook: parsing allow CIDR %q: %w", raw, err)
		}
		h.allowed = append(h.allowed, prefix.Masked())
	}

	app, err := ctx.AppIfConfigured("tblocker")
	if err != nil {
		return fmt.Errorf("tblocker_webhook: configure the global tblocker app: %w", err)
	}
	var ok bool
	h.app, ok = app.(*App)
	if !ok {
		return fmt.Errorf("tblocker_webhook: unexpected app type %T", app)
	}
	return nil
}

// ServeHTTP validates and stores the action report. The route itself should use
// an unguessable path and be bound to a private Docker network only.
func (h Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil
	}
	remote, err := remoteAddr(r.RemoteAddr)
	if err != nil || !h.isAllowed(remote) {
		w.WriteHeader(http.StatusForbidden)
		return nil
	}

	var report remnaReport
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.MaxBody))
	if err := decoder.Decode(&report); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return nil
	}
	addr, err := netip.ParseAddr(report.ActionReport.IP)
	if err != nil || !addr.IsValid() || addr.IsUnspecified() {
		w.WriteHeader(http.StatusBadRequest)
		return nil
	}

	expiresAt, err := h.expiration(report)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil
	}
	h.app.Ban(addr, expiresAt)
	if h.app.logger != nil {
		h.app.logger.Info("torrent client temporarily blocked",
			zap.String("client_ip", addr.String()),
			zap.Time("expires_at", expiresAt),
		)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h Webhook) expiration(report remnaReport) (time.Time, error) {
	now := h.app.clock()
	expiresAt := report.ActionReport.WillUnblockAt
	if expiresAt.IsZero() {
		duration := time.Duration(h.app.DefaultTTL)
		if report.ActionReport.BlockDuration > 0 {
			duration = time.Duration(report.ActionReport.BlockDuration) * time.Second
		}
		expiresAt = now.Add(duration)
	}
	if !expiresAt.After(now) {
		return time.Time{}, fmt.Errorf("expiration is not in the future")
	}
	maximum := now.Add(time.Duration(h.app.MaxTTL))
	if expiresAt.After(maximum) {
		expiresAt = maximum
	}
	return expiresAt, nil
}

func (h Webhook) isAllowed(addr netip.Addr) bool {
	for _, prefix := range h.allowed {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteAddr(raw string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(host)
}

type remnaReport struct {
	ActionReport struct {
		IP            string    `json:"ip"`
		BlockDuration int64     `json:"blockDuration"`
		WillUnblockAt time.Time `json:"willUnblockAt"`
	} `json:"actionReport"`
}

func parseApp(d *caddyfile.Dispenser, _ any) (any, error) {
	d.Next()
	app := new(App)
	for d.NextBlock(0) {
		var value string
		switch d.Val() {
		case "default_ttl":
			if !d.AllArgs(&value) {
				return nil, d.ArgErr()
			}
			duration, err := time.ParseDuration(value)
			if err != nil {
				return nil, d.Errf("parsing default_ttl: %v", err)
			}
			app.DefaultTTL = caddy.Duration(duration)
		case "max_ttl":
			if !d.AllArgs(&value) {
				return nil, d.ArgErr()
			}
			duration, err := time.ParseDuration(value)
			if err != nil {
				return nil, d.Errf("parsing max_ttl: %v", err)
			}
			app.MaxTTL = caddy.Duration(duration)
		default:
			return nil, d.Errf("unrecognized tblocker subdirective %q", d.Val())
		}
	}
	return httpcaddyfile.App{Name: "tblocker", Value: caddyconfig.JSON(app, nil)}, nil
}

func parseHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next()
	handler := new(Handler)
	for h.NextBlock(0) {
		switch h.Val() {
		case "status":
			var raw string
			if !h.AllArgs(&raw) {
				return nil, h.ArgErr()
			}
			var err error
			if _, err = fmt.Sscan(raw, &handler.StatusCode); err != nil {
				return nil, h.Errf("parsing status: %v", err)
			}
		default:
			return nil, h.Errf("unrecognized tblocker subdirective %q", h.Val())
		}
	}
	return handler, nil
}

func parseWebhook(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next()
	webhook := new(Webhook)
	for h.NextBlock(0) {
		switch h.Val() {
		case "allow":
			values := h.RemainingArgs()
			if len(values) == 0 {
				return nil, h.ArgErr()
			}
			webhook.Allow = append(webhook.Allow, values...)
		case "max_body":
			var raw string
			if !h.AllArgs(&raw) {
				return nil, h.ArgErr()
			}
			size, err := humanize.ParseBytes(raw)
			if err != nil {
				return nil, h.Errf("parsing max_body: %v", err)
			}
			webhook.MaxBody = int64(size)
		default:
			return nil, h.Errf("unrecognized tblocker_webhook subdirective %q", h.Val())
		}
	}
	return webhook, nil
}

var (
	_ caddy.App                   = (*App)(nil)
	_ caddy.Provisioner           = (*App)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Webhook)(nil)
	_ caddy.Provisioner           = (*Webhook)(nil)
)
