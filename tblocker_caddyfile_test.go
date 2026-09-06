package tblocker

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"

	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// adapt runs a Caddyfile through the same adapter `caddy adapt` uses.
func adapt(t *testing.T, config string) map[string]any {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("the caddyfile adapter is not registered")
	}
	result, warnings, err := adapter.Adapt([]byte(config), nil)
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	for _, warning := range warnings {
		if !strings.Contains(warning.Message, "not formatted") {
			t.Fatalf("unexpected warning: %s", warning.Message)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func adaptError(t *testing.T, config string) string {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	_, _, err := adapter.Adapt([]byte(config), nil)
	if err == nil {
		t.Fatal("expected the config to be rejected")
	}
	return err.Error()
}

// findHandlers walks the adapted config and collects every handler of one type.
func findHandlers(node any, id string, found *[]map[string]any) {
	switch typed := node.(type) {
	case map[string]any:
		if typed["handler"] == id {
			*found = append(*found, typed)
		}
		for _, value := range typed {
			findHandlers(value, id, found)
		}
	case []any:
		for _, value := range typed {
			findHandlers(value, id, found)
		}
	}
}

func onlyHandler(t *testing.T, config, id string) map[string]any {
	t.Helper()
	var found []map[string]any
	findHandlers(adapt(t, config), id, &found)
	if len(found) != 1 {
		t.Fatalf("found %d %q handlers, want 1", len(found), id)
	}
	return found[0]
}

const globals = "{\n\tauto_https off\n\ttblocker {\n\t\tdefault_ttl 1m\n\t}\n}\n"

// The ban check has to be ordered ahead of every handler that can end the
// chain on its own, so a plain site block works without a route wrapper. This
// is the regression guard for the ordering bug that made the directive dead
// code whenever it shared a block with respond or handle.
func TestDirectiveRunsBeforeTerminalHandlers(t *testing.T) {
	config := globals + `
http://:8080 {
	tblocker
	handle /sub* {
		respond "sub" 200
	}
	respond "root" 200
}
`
	decoded := adapt(t, config)
	servers := decoded["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)

	var firstHandler string
	for _, server := range servers {
		routes := server.(map[string]any)["routes"].([]any)
		var handlers []map[string]any
		findHandlers(routes[0], "tblocker", &handlers)
		if len(handlers) == 1 {
			firstHandler = "tblocker"
		}
	}
	if firstHandler != "tblocker" {
		body, _ := json.Marshal(servers)
		t.Fatalf("tblocker is not the first route; got %s", body)
	}
}

func TestHandlerOptionsAdapt(t *testing.T) {
	handler := onlyHandler(t, globals+`
http://:8080 {
	tblocker {
		status 401
		drop_existing
		header WWW-Authenticate `+"`"+`Basic realm="restricted"`+"`"+`
		header X-Multi one
		header X-Multi two
		header -Cache-Control
	}
}
`, "tblocker")

	if handler["status_code"] != float64(401) {
		t.Fatalf("status_code=%v", handler["status_code"])
	}
	if handler["drop_existing"] != true {
		t.Fatalf("drop_existing=%v", handler["drop_existing"])
	}
	headers := handler["headers"].(map[string]any)
	if got := headers["Www-Authenticate"].([]any); len(got) != 1 || got[0] != `Basic realm="restricted"` {
		t.Fatalf("Www-Authenticate=%v", got)
	}
	if got := headers["X-Multi"].([]any); len(got) != 2 {
		t.Fatalf("a repeated name must keep both values, got %v", got)
	}
	if got := headers["Cache-Control"].([]any); len(got) != 0 {
		t.Fatalf("Cache-Control should be an empty list meaning removal, got %v", got)
	}
}

func TestDropExistingAcceptsOnAndOff(t *testing.T) {
	for _, testCase := range []struct {
		literal string
		want    any
	}{
		{"drop_existing", true},
		{"drop_existing on", true},
		{"drop_existing off", nil}, // false is omitted from JSON
	} {
		handler := onlyHandler(t, globals+"http://:8080 {\n\ttblocker {\n\t\t"+testCase.literal+"\n\t}\n}\n", "tblocker")
		if handler["drop_existing"] != testCase.want {
			t.Fatalf("%q gave drop_existing=%v want %v", testCase.literal, handler["drop_existing"], testCase.want)
		}
	}
}

func TestCaddyfileRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		mustSay string
	}{
		{"non-numeric status", "tblocker {\n\t\tstatus abc\n\t}", "parsing status"},
		{"trailing junk on status", "tblocker {\n\t\tstatus 403abc\n\t}", "parsing status"},
		{"status out of range", "tblocker {\n\t\tstatus 200\n\t}", "HTTP error status"},
		{"unknown subdirective", "tblocker {\n\t\tbogus 1\n\t}", "unrecognized"},
		{"header with no arguments", "tblocker {\n\t\theader\n\t}", "header takes"},
		{"header with three arguments", "tblocker {\n\t\theader A b c\n\t}", "header takes"},
		{"deletion written with a value", "tblocker {\n\t\theader -X-Foo bar\n\t}", "to remove a header"},
		{"invalid header name", "tblocker {\n\t\theader \"X Foo\" bar\n\t}", "invalid header name"},
		{"bare dash", "tblocker {\n\t\theader -\n\t}", "header name must not be empty"},
		{"drop_existing with junk", "tblocker {\n\t\tdrop_existing maybe\n\t}", "on/off"},
		{"webhook with no allow argument", "tblocker_webhook {\n\t\tallow\n\t}", "wrong argument count"},
		{"webhook with a bad size", "tblocker_webhook {\n\t\tallow 127.0.0.0/8\n\t\tmax_body huge\n\t}", "parsing max_body"},
		{"admin with an unknown subdirective", "tblocker_admin {\n\t\tbogus 1\n\t}", "unrecognized"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := globals + "http://:8080 {\n\t" + testCase.body + "\n}\n"
			message := adaptError(t, config)
			if !strings.Contains(message, testCase.mustSay) {
				t.Fatalf("got %q, want it to mention %q", message, testCase.mustSay)
			}
		})
	}
}

func TestGlobalOptionsAdapt(t *testing.T) {
	decoded := adapt(t, `{
	auto_https off
	tblocker {
		default_ttl 30s
		max_ttl 12h
		sweep_interval 5m
		max_entries 4096
		ipv4_prefix 24
		ipv6_prefix 64
		ignore 127.0.0.0/8 192.168.243.0/28
	}
}
http://:8080 {
	tblocker
}
`)
	app := decoded["apps"].(map[string]any)["tblocker"].(map[string]any)
	if app["default_ttl"] != float64(30*time.Second) {
		t.Fatalf("default_ttl=%v", app["default_ttl"])
	}
	if app["max_ttl"] != float64(12*time.Hour) {
		t.Fatalf("max_ttl=%v", app["max_ttl"])
	}
	if app["sweep_interval"] != float64(5*time.Minute) {
		t.Fatalf("sweep_interval=%v", app["sweep_interval"])
	}
	if app["max_entries"] != float64(4096) {
		t.Fatalf("max_entries=%v", app["max_entries"])
	}
	if app["ipv4_prefix"] != float64(24) || app["ipv6_prefix"] != float64(64) {
		t.Fatalf("prefixes=%v/%v", app["ipv4_prefix"], app["ipv6_prefix"])
	}
	if got := app["ignore"].([]any); len(got) != 2 {
		t.Fatalf("ignore=%v", got)
	}
}

func TestGlobalOptionRejectsStrayArgument(t *testing.T) {
	if message := adaptError(t, "{\n\tauto_https off\n\ttblocker stray {\n\t\tdefault_ttl 1m\n\t}\n}\nhttp://:8080 {\n\trespond \"x\"\n}\n"); message == "" {
		t.Fatal("expected an error")
	}
}

// A widened ban has to be reported as a network, not as a bare address that
// looks like a single host.
func TestBansReportTheNetworkWhenWidened(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := newTestApp(now)
	app.v4Bits = 24
	app.v6Bits = 64
	app.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Minute))
	app.Ban(netip.MustParseAddr("2001:db8:1:2::dead"), now.Add(time.Minute))

	byIP := map[string]BanEntry{}
	for _, entry := range app.Bans() {
		byIP[entry.IP] = entry
	}
	if got := byIP["203.0.113.0"].Network; got != "203.0.113.0/24" {
		t.Fatalf("ipv4 network=%q", got)
	}
	if got := byIP["2001:db8:1:2::"].Network; got != "2001:db8:1:2::/64" {
		t.Fatalf("ipv6 network=%q", got)
	}

	// With the defaults every ban is a single host and the field stays absent.
	plain := newTestApp(now)
	plain.Ban(netip.MustParseAddr("203.0.113.42"), now.Add(time.Minute))
	if got := plain.Bans()[0].Network; got != "" {
		t.Fatalf("network=%q, want empty for a single host", got)
	}
}

// The allow list is validated when the module is provisioned, which the
// Caddyfile adapter does not do, so it needs its own coverage.
func TestSourceGuardsAreValidatedOnProvision(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	for _, testCase := range []struct {
		name    string
		module  caddy.Provisioner
		mustSay string
	}{
		{"webhook without allow", &Webhook{}, "at least one allow CIDR"},
		{"webhook with a bad cidr", &Webhook{Allow: []string{"999.0.0.0/8"}}, "parsing allow CIDR"},
		{"webhook with a bare address", &Webhook{Allow: []string{"192.168.243.3"}}, "parsing allow CIDR"},
		{"webhook with a negative body cap", &Webhook{Allow: []string{"127.0.0.0/8"}, MaxBody: -1}, "max_body must be positive"},
		{"admin without allow", &Admin{}, "at least one allow CIDR"},
		{"admin with a bad cidr", &Admin{Allow: []string{"999.0.0.0/8"}}, "parsing allow CIDR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.module.Provision(ctx)
			if err == nil || !strings.Contains(err.Error(), testCase.mustSay) {
				t.Fatalf("err=%v, want it to mention %q", err, testCase.mustSay)
			}
		})
	}
}

var _ = httpcaddyfile.App{}
