// Package tblocker provides a Caddy-native TTL blocklist populated by the
// Remnawave torrent-blocker webhook.
package tblocker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
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

const (
	defaultTTL           = time.Minute
	defaultMaxTTL        = 24 * time.Hour
	defaultSweepInterval = time.Minute
	defaultMaxEntries    = 100_000
	defaultMaxBody       = 64 << 10
)

func init() {
	caddy.RegisterModule(new(App))
	caddy.RegisterModule(Handler{})
	caddy.RegisterModule(Webhook{})
	caddy.RegisterModule(Admin{})

	httpcaddyfile.RegisterGlobalOption("tblocker", parseApp)
	httpcaddyfile.RegisterHandlerDirective("tblocker", parseHandler)
	httpcaddyfile.RegisterHandlerDirective("tblocker_webhook", parseWebhook)
	httpcaddyfile.RegisterHandlerDirective("tblocker_admin", parseAdmin)

	// The ban check has to run before every directive that can terminate the
	// middleware chain on its own, which includes handle, handle_path, route,
	// respond, error, abort and reverse_proxy. Ordering it before redir puts it
	// ahead of all of them, so a plain
	//
	//	tblocker
	//	handle /x { ... }
	//
	// site block is checked first, without needing an explicit route wrapper.
	httpcaddyfile.RegisterDirectiveOrder("tblocker", httpcaddyfile.Before, "redir")
	httpcaddyfile.RegisterDirectiveOrder("tblocker_webhook", httpcaddyfile.Before, "respond")
	httpcaddyfile.RegisterDirectiveOrder("tblocker_admin", httpcaddyfile.Before, "respond")
}

// BanEntry is one live blocklist record as reported by the admin handler.
type BanEntry struct {
	IP        string    `json:"ip"`
	ExpiresAt time.Time `json:"expires_at"`
}

// App is the process-local ban store shared by the HTTP handlers. Entries do
// not survive a Caddy reload, intentionally keeping the failure mode bounded.
type App struct {
	// DefaultTTL is the ban lifetime used when the webhook supplies no usable
	// duration of its own.
	DefaultTTL caddy.Duration `json:"default_ttl,omitempty"`
	// MaxTTL caps every accepted ban lifetime.
	MaxTTL caddy.Duration `json:"max_ttl,omitempty"`
	// SweepInterval is how often expired entries are purged in the background.
	SweepInterval caddy.Duration `json:"sweep_interval,omitempty"`
	// MaxEntries bounds the store; further bans are dropped once it is full.
	MaxEntries int `json:"max_entries,omitempty"`
	// IPv4Prefix and IPv6Prefix widen a ban to a whole network. Both default to
	// a single host, 32 and 128.
	IPv4Prefix int `json:"ipv4_prefix,omitempty"`
	IPv6Prefix int `json:"ipv6_prefix,omitempty"`
	// Ignore lists CIDRs that must never be banned, as a safety net against a
	// report that carries an infrastructure address.
	Ignore []string `json:"ignore,omitempty"`

	ignore []netip.Prefix
	v4Bits int
	v6Bits int

	// active tracks in-flight requests so that a new ban can tear down the
	// connections that are already running, not just refuse the next one.
	active activeRequests

	mu     sync.RWMutex
	bans   map[netip.Addr]time.Time
	logger *zap.Logger
	now    func() time.Time

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// activeRequests maps a store key to the cancel function of every request
// currently being served for it.
type activeRequests struct {
	mu     sync.Mutex
	nextID uint64
	byAddr map[netip.Addr]map[uint64]context.CancelFunc
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
		a.DefaultTTL = caddy.Duration(defaultTTL)
	}
	if a.MaxTTL == 0 {
		a.MaxTTL = caddy.Duration(defaultMaxTTL)
	}
	if a.SweepInterval == 0 {
		a.SweepInterval = caddy.Duration(defaultSweepInterval)
	}
	if a.MaxEntries == 0 {
		a.MaxEntries = defaultMaxEntries
	}
	if a.IPv4Prefix == 0 {
		a.IPv4Prefix = 32
	}
	if a.IPv6Prefix == 0 {
		a.IPv6Prefix = 128
	}
	if a.DefaultTTL <= 0 || a.MaxTTL <= 0 || a.DefaultTTL > a.MaxTTL {
		return fmt.Errorf("tblocker: require 0 < default_ttl <= max_ttl")
	}
	if a.SweepInterval <= 0 {
		return fmt.Errorf("tblocker: sweep_interval must be positive")
	}
	if a.MaxEntries < 0 {
		return fmt.Errorf("tblocker: max_entries must not be negative")
	}
	if a.IPv4Prefix < 1 || a.IPv4Prefix > 32 {
		return fmt.Errorf("tblocker: ipv4_prefix must be between 1 and 32")
	}
	if a.IPv6Prefix < 1 || a.IPv6Prefix > 128 {
		return fmt.Errorf("tblocker: ipv6_prefix must be between 1 and 128")
	}
	for _, raw := range a.Ignore {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return fmt.Errorf("tblocker: parsing ignore CIDR %q: %w", raw, err)
		}
		a.ignore = append(a.ignore, prefix.Masked())
	}

	a.v4Bits = a.IPv4Prefix
	a.v6Bits = a.IPv6Prefix
	a.bans = make(map[netip.Addr]time.Time)
	a.active.byAddr = make(map[netip.Addr]map[uint64]context.CancelFunc)
	a.logger = ctx.Logger()
	a.now = time.Now
	return nil
}

// Start launches the background sweeper that keeps the store from growing
// without bound: a banned address usually stops connecting, so lazy expiry on
// lookup alone would retain its entry until the next reload.
func (a *App) Start() error {
	a.done = make(chan struct{})
	a.wg.Add(1)
	go a.sweepLoop(time.Duration(a.SweepInterval))
	return nil
}

// Stop shuts the background sweeper down.
func (a *App) Stop() error {
	if a.done != nil {
		a.closeOnce.Do(func() { close(a.done) })
	}
	a.wg.Wait()
	return nil
}

func (a *App) sweepLoop(interval time.Duration) {
	defer a.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			if removed := a.Sweep(); removed > 0 && a.logger != nil {
				a.logger.Debug("expired bans purged", zap.Int("removed", removed))
			}
		}
	}
}

// Sweep removes every expired entry and reports how many were dropped.
func (a *App) Sweep() int {
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sweepLocked(now)
}

func (a *App) sweepLocked(now time.Time) int {
	removed := 0
	for addr, expiresAt := range a.bans {
		if !expiresAt.After(now) {
			delete(a.bans, addr)
			removed++
		}
	}
	return removed
}

// key normalizes an address into a store key, applying the configured ban
// prefix so that both sides of a comparison are always masked identically.
func (a *App) key(addr netip.Addr) netip.Addr {
	addr = addr.Unmap().WithZone("")
	bits := a.v4Bits
	if addr.Is6() {
		bits = a.v6Bits
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return addr
	}
	return prefix.Addr()
}

// Ignored reports whether addr is protected from ever being banned.
func (a *App) Ignored(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	for _, prefix := range a.ignore {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Ban records addr until expiresAt. It reports whether the entry was stored
// and how many in-flight requests were torn down as a result. A later
// expiration replaces an earlier one, so a shorter duplicate report never
// unblocks a client prematurely.
func (a *App) Ban(addr netip.Addr, expiresAt time.Time) (stored bool, dropped int) {
	if a.Ignored(addr) {
		return false, 0
	}
	key := a.key(addr)
	if !a.store(key, expiresAt) {
		return false, 0
	}
	// Deliberately outside the store lock: cancelling a request runs the
	// handler's deferred release, which takes the registry lock.
	return true, a.dropActive(key)
}

func (a *App) store(key netip.Addr, expiresAt time.Time) bool {
	now := a.clock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if current, ok := a.bans[key]; ok {
		if expiresAt.After(current) {
			a.bans[key] = expiresAt
		}
		return true
	}
	if a.MaxEntries > 0 && len(a.bans) >= a.MaxEntries {
		a.sweepLocked(now)
		if len(a.bans) >= a.MaxEntries {
			return false
		}
	}
	a.bans[key] = expiresAt
	return true
}

// trackRequest registers an in-flight request and returns its deregistration
// function, which the caller must defer.
func (a *App) trackRequest(addr netip.Addr, cancel context.CancelFunc) func() {
	key := a.key(addr)

	a.active.mu.Lock()
	id := a.active.nextID
	a.active.nextID++
	set, ok := a.active.byAddr[key]
	if !ok {
		set = make(map[uint64]context.CancelFunc)
		a.active.byAddr[key] = set
	}
	set[id] = cancel
	a.active.mu.Unlock()

	return func() {
		a.active.mu.Lock()
		if set, ok := a.active.byAddr[key]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(a.active.byAddr, key)
			}
		}
		a.active.mu.Unlock()
	}
}

// dropActive cancels every tracked request for key and reports how many.
// Cancelling the request context is what tears the tunnel down: Caddy's
// reverse proxy closes the upstream connection when the request context is
// done, including for hijacked WebSocket and HTTPUpgrade streams. Cancelling
// per request rather than closing the TCP connection matters behind a CDN,
// where one HTTP/2 connection can carry several unrelated clients.
func (a *App) dropActive(key netip.Addr) int {
	a.active.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.active.byAddr[key]))
	for _, cancel := range a.active.byAddr[key] {
		cancels = append(cancels, cancel)
	}
	a.active.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// IsBanned reports whether addr has a live ban and lazily removes expired data.
func (a *App) IsBanned(addr netip.Addr) bool {
	if a.Ignored(addr) {
		return false
	}
	key := a.key(addr)
	now := a.clock()

	a.mu.RLock()
	expiresAt, ok := a.bans[key]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if expiresAt.After(now) {
		return true
	}

	a.mu.Lock()
	if current, exists := a.bans[key]; exists && !current.After(now) {
		delete(a.bans, key)
	}
	a.mu.Unlock()
	return false
}

// Unban drops the entry covering addr and reports whether one was still live.
func (a *App) Unban(addr netip.Addr) bool {
	key := a.key(addr)
	now := a.clock()

	a.mu.Lock()
	defer a.mu.Unlock()
	expiresAt, ok := a.bans[key]
	delete(a.bans, key)
	return ok && expiresAt.After(now)
}

// Flush drops every entry and reports how many live ones were removed.
func (a *App) Flush() int {
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	removed := 0
	for addr, expiresAt := range a.bans {
		if expiresAt.After(now) {
			removed++
		}
		delete(a.bans, addr)
	}
	return removed
}

// Bans returns the live entries, sorted by address for stable output.
func (a *App) Bans() []BanEntry {
	now := a.clock()
	a.mu.RLock()
	entries := make([]BanEntry, 0, len(a.bans))
	for addr, expiresAt := range a.bans {
		if expiresAt.After(now) {
			entries = append(entries, BanEntry{IP: addr.String(), ExpiresAt: expiresAt.UTC()})
		}
	}
	a.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].IP < entries[j].IP })
	return entries
}

func (a *App) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func lookupApp(ctx caddy.Context, module string) (*App, error) {
	app, err := ctx.AppIfConfigured("tblocker")
	if err != nil {
		return nil, fmt.Errorf("%s: configure the global tblocker app: %w", module, err)
	}
	typed, ok := app.(*App)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected app type %T", module, app)
	}
	return typed, nil
}

// Handler blocks a request before it reaches the next HTTP handler when Caddy's
// trusted-proxy-aware client_ip is currently banned.
type Handler struct {
	// StatusCode is the response returned to a blocked client.
	StatusCode int `json:"status_code,omitempty"`
	// DropExisting tears down the requests already in flight for an address
	// when it gets banned, instead of only refusing its next one.
	DropExisting bool `json:"drop_existing,omitempty"`

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
	if err := validateStatus(h.StatusCode); err != nil {
		return fmt.Errorf("tblocker: %w", err)
	}
	app, err := lookupApp(ctx, "tblocker")
	if err != nil {
		return err
	}
	h.app = app
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
		if !h.DropExisting {
			return next.ServeHTTP(w, r)
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		release := h.app.trackRequest(addr, cancel)
		defer release()
		return next.ServeHTTP(w, r.WithContext(ctx))
	}
	if h.app.logger != nil {
		h.app.logger.Debug("request denied", zap.String("client_ip", addr.String()))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(h.StatusCode)
	return nil
}

// Webhook accepts a torrent-blocker action report from a Remnawave node and
// puts its client IP into the shared TTL blocklist. The node calls this
// directly when its Xray fires the rule; the panel is not involved.
type Webhook struct {
	// Allow lists the CIDRs permitted to submit reports.
	Allow []string `json:"allow,omitempty"`
	// MaxBody caps the accepted request body.
	MaxBody int64 `json:"max_body,omitempty"`

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
		h.MaxBody = defaultMaxBody
	}
	if h.MaxBody < 0 {
		return fmt.Errorf("tblocker_webhook: max_body must be positive")
	}
	allowed, err := parseAllow(h.Allow, "tblocker_webhook")
	if err != nil {
		return err
	}
	h.allowed = allowed

	app, err := lookupApp(ctx, "tblocker_webhook")
	if err != nil {
		return err
	}
	h.app = app
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
	if !sourcePermitted(r.RemoteAddr, h.allowed) {
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

	expiresAt := h.expiration(report)
	stored, dropped := h.app.Ban(addr, expiresAt)
	if !stored {
		// The address is either explicitly protected or the store is full.
		// Both are operational facts about this deployment, not client errors,
		// so the report is still acknowledged.
		if h.app.logger != nil {
			h.app.logger.Warn("torrent report not stored",
				zap.String("client_ip", addr.String()),
				zap.Bool("ignored", h.app.Ignored(addr)),
			)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if h.app.logger != nil {
		h.app.logger.Info("torrent client temporarily blocked",
			zap.String("client_ip", addr.String()),
			zap.Time("expires_at", expiresAt),
			zap.Int("dropped_requests", dropped),
		)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// expiration prefers the relative blockDuration over the absolute
// willUnblockAt, because the latter is stamped with the node's clock: any skew
// between the node and Caddy would shift the ban or, if the node runs behind,
// void it entirely.
func (h Webhook) expiration(report remnaReport) time.Time {
	now := h.app.clock()

	var expiresAt time.Time
	switch {
	case report.ActionReport.BlockDuration > 0:
		expiresAt = now.Add(time.Duration(report.ActionReport.BlockDuration) * time.Second)
	case report.ActionReport.WillUnblockAt.After(now):
		expiresAt = report.ActionReport.WillUnblockAt
	default:
		expiresAt = now.Add(time.Duration(h.app.DefaultTTL))
	}

	if maximum := now.Add(time.Duration(h.app.MaxTTL)); expiresAt.After(maximum) {
		expiresAt = maximum
	}
	return expiresAt
}

// Admin exposes the live blocklist for inspection and manual release.
// The panel releases an IP by calling the node's own nftables endpoint, which
// never reaches Caddy, so lifting a ban early has to happen here.
type Admin struct {
	// Allow lists the CIDRs permitted to inspect and modify the blocklist.
	Allow []string `json:"allow,omitempty"`

	allowed []netip.Prefix
	app     *App
}

// CaddyModule returns the module information.
func (Admin) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.tblocker_admin",
		New: func() caddy.Module { return new(Admin) },
	}
}

// Provision validates the admin source ranges.
func (h *Admin) Provision(ctx caddy.Context) error {
	allowed, err := parseAllow(h.Allow, "tblocker_admin")
	if err != nil {
		return err
	}
	h.allowed = allowed

	app, err := lookupApp(ctx, "tblocker_admin")
	if err != nil {
		return err
	}
	h.app = app
	return nil
}

// ServeHTTP lists bans on GET and releases them on DELETE. A DELETE without an
// "ip" query parameter clears the whole blocklist.
func (h Admin) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if !sourcePermitted(r.RemoteAddr, h.allowed) {
		w.WriteHeader(http.StatusForbidden)
		return nil
	}

	switch r.Method {
	case http.MethodGet:
		entries := h.app.Bans()
		return writeJSON(w, http.StatusOK, map[string]any{
			"count": len(entries),
			"bans":  entries,
		})

	case http.MethodDelete:
		raw := r.URL.Query().Get("ip")
		if raw == "" {
			removed := h.app.Flush()
			if h.app.logger != nil {
				h.app.logger.Info("blocklist flushed", zap.Int("removed", removed))
			}
			return writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid ip"})
		}
		if !h.app.Unban(addr) {
			return writeJSON(w, http.StatusNotFound, map[string]any{"error": "no live ban for this ip"})
		}
		if h.app.logger != nil {
			h.app.logger.Info("ban released", zap.String("client_ip", addr.String()))
		}
		return writeJSON(w, http.StatusOK, map[string]any{"removed": 1})

	default:
		w.Header().Set("Allow", "GET, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}

func parseAllow(raw []string, module string) ([]netip.Prefix, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: at least one allow CIDR is required", module)
	}
	prefixes := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("%s: parsing allow CIDR %q: %w", module, value, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// sourcePermitted checks the real TCP peer and never a forwarding header:
// these routes are meant to be reachable from the private Docker network only.
func sourcePermitted(remoteAddr string, allowed []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range allowed {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func validateStatus(status int) error {
	if status < 400 || status > 599 {
		return fmt.Errorf("status must be an HTTP error status (400-599), got %d", status)
	}
	return nil
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
	if d.NextArg() {
		return nil, d.ArgErr()
	}
	app := new(App)
	for d.NextBlock(0) {
		var value string
		switch option := d.Val(); option {
		case "default_ttl", "max_ttl", "sweep_interval":
			if !d.AllArgs(&value) {
				return nil, d.ArgErr()
			}
			duration, err := time.ParseDuration(value)
			if err != nil {
				return nil, d.Errf("parsing %s: %v", option, err)
			}
			switch option {
			case "default_ttl":
				app.DefaultTTL = caddy.Duration(duration)
			case "max_ttl":
				app.MaxTTL = caddy.Duration(duration)
			case "sweep_interval":
				app.SweepInterval = caddy.Duration(duration)
			}
		case "max_entries", "ipv4_prefix", "ipv6_prefix":
			if !d.AllArgs(&value) {
				return nil, d.ArgErr()
			}
			number, err := strconv.Atoi(value)
			if err != nil {
				return nil, d.Errf("parsing %s: %v", option, err)
			}
			switch option {
			case "max_entries":
				app.MaxEntries = number
			case "ipv4_prefix":
				app.IPv4Prefix = number
			case "ipv6_prefix":
				app.IPv6Prefix = number
			}
		case "ignore":
			values := d.RemainingArgs()
			if len(values) == 0 {
				return nil, d.ArgErr()
			}
			app.Ignore = append(app.Ignore, values...)
		default:
			return nil, d.Errf("unrecognized tblocker subdirective %q", option)
		}
	}
	return httpcaddyfile.App{Name: "tblocker", Value: caddyconfig.JSON(app, nil)}, nil
}

func parseHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	// RegisterHandlerDirective already consumed the directive name; advance past
	// it so NextBlock finds the opening brace.
	h.Next()
	handler := new(Handler)
	for h.NextBlock(0) {
		switch h.Val() {
		case "status":
			var raw string
			if !h.AllArgs(&raw) {
				return nil, h.ArgErr()
			}
			status, err := strconv.Atoi(raw)
			if err != nil {
				return nil, h.Errf("parsing status: %v", err)
			}
			if err := validateStatus(status); err != nil {
				return nil, h.Err(err.Error())
			}
			handler.StatusCode = status
		case "drop_existing":
			values := h.RemainingArgs()
			switch {
			case len(values) == 0:
				handler.DropExisting = true
			case len(values) == 1 && (values[0] == "on" || values[0] == "off"):
				handler.DropExisting = values[0] == "on"
			default:
				return nil, h.Errf("drop_existing takes no argument, or on/off")
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

func parseAdmin(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next()
	admin := new(Admin)
	for h.NextBlock(0) {
		switch h.Val() {
		case "allow":
			values := h.RemainingArgs()
			if len(values) == 0 {
				return nil, h.ArgErr()
			}
			admin.Allow = append(admin.Allow, values...)
		default:
			return nil, h.Errf("unrecognized tblocker_admin subdirective %q", h.Val())
		}
	}
	return admin, nil
}

var (
	_ caddy.App                   = (*App)(nil)
	_ caddy.Provisioner           = (*App)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Webhook)(nil)
	_ caddy.Provisioner           = (*Webhook)(nil)
	_ caddyhttp.MiddlewareHandler = (*Admin)(nil)
	_ caddy.Provisioner           = (*Admin)(nil)
)
