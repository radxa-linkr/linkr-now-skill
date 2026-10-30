package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// These acceptance tests only use loopback httptest servers. They do not need
// Linkr hardware, mDNS, a browser, a running proxy, or an internet connection.
type mockDevice struct {
	server *httptest.Server
	mu     sync.Mutex
	routes map[string]any
	gets   map[string]int
}

func newMockDevice(t *testing.T, routes map[string]any) *mockDevice {
	t.Helper()
	d := &mockDevice{routes: routes, gets: make(map[string]int)}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.gets[r.URL.Path]++
		value, ok := d.routes[r.URL.Path]
		d.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if handler, ok := value.(http.HandlerFunc); ok {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *mockDevice) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gets[path]
}

func okData(data any) any { return map[string]any{"code": 0, "data": data} }

func testInfo(name string) map[string]any {
	return map[string]any{
		"deviceName": name, "status": "IDLE", "onlineTime": "123.45",
		"heartbeat": 1793230000, "version": "v1.6.0-beta2",
	}
}

func invoke(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, args, &stdout, &stderr)
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON output: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	return code, payload, stderr.String()
}

func devices(t *testing.T, payload map[string]any) []any {
	t.Helper()
	items, ok := payload["devices"].([]any)
	if !ok {
		t.Fatalf("devices must be a JSON array, got %#v", payload["devices"])
	}
	return items
}

func TestInfoReadsUptimeAndPreservesDeviceClock(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Test Linkr"))})
	code, payload, stderr := invoke(t, "info", "--json", d.server.URL)
	if code != 0 {
		t.Fatalf("info exit=%d stderr=%q payload=%#v", code, stderr, payload)
	}
	items := devices(t, payload)
	if len(items) != 1 {
		t.Fatalf("expected one device, got %#v", items)
	}
	device := items[0].(map[string]any)
	if device["status"] != "IDLE" || device["onlineTimeSeconds"] != 123.45 {
		t.Fatalf("incorrect info conversion: %#v", device)
	}
	if device["heartbeat"] != float64(1793230000) || device["receivedAt"] == nil {
		t.Fatalf("device heartbeat and local receive time must stay separate: %#v", device)
	}
}

func TestScanDeduplicatesDomainsWithoutCollapsingSharedIDs(t *testing.T) {
	first := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("First"))})
	second := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Second"))})
	firstDomain := strings.TrimPrefix(first.server.URL, "http://")
	secondDomain := strings.TrimPrefix(second.server.URL, "http://")
	gateway := newMockDevice(t, map[string]any{"/linkr/list": okData(map[string]any{"list": []any{
		map[string]any{"id": "linkr", "domain": firstDomain},
		map[string]any{"id": "linkr", "domain": secondDomain},
		map[string]any{"id": "linkr", "domain": firstDomain},
	}})})
	code, payload, stderr := invoke(t, "scan", "--via", gateway.server.URL, "--no-usb", "--no-mdns", "--json")
	if code != 0 {
		t.Fatalf("scan exit=%d stderr=%q payload=%#v", code, stderr, payload)
	}
	items := devices(t, payload)
	if len(items) != 2 {
		t.Fatalf("same shared id must not collapse distinct domains: %#v", items)
	}
	names := map[any]bool{}
	for _, item := range items {
		names[item.(map[string]any)["name"]] = true
	}
	if !names["First"] || !names["Second"] || first.count("/linkr/info") != 1 {
		t.Fatalf("unexpected deduplication: names=%v firstInfoRequests=%d", names, first.count("/linkr/info"))
	}
}

func TestScanPartialFailureRetainsConfirmedDevice(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Network"))})
	usb := newMockDevice(t, map[string]any{"/linkr/info": map[string]any{"code": 1, "message": "not ready"}})
	code, payload, stderr := invoke(t, "scan", "--no-network", "--target", d.server.URL,
		"--usb-host", usb.server.URL, "--json")
	if code != 0 {
		t.Fatalf("partial success should exit zero; got %d stderr=%q payload=%#v", code, stderr, payload)
	}
	confirmed := false
	for _, item := range devices(t, payload) {
		confirmed = confirmed || item.(map[string]any)["connected"] == true
	}
	if !confirmed || len(payload["errors"].([]any)) == 0 {
		t.Fatalf("partial failure must preserve success and its diagnostic: %#v", payload)
	}
}

func TestScanUSBAndNetworkStayIndependent(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Same physical device"))})
	code, payload, stderr := invoke(t, "scan", "--no-network", "--target", d.server.URL,
		"--usb-host", d.server.URL, "--json")
	if code != 0 {
		t.Fatalf("scan exit=%d stderr=%q payload=%#v", code, stderr, payload)
	}
	sources := map[any]bool{}
	for _, item := range devices(t, payload) {
		sources[item.(map[string]any)["source"]] = true
	}
	if !sources["usb"] || !sources["network"] {
		t.Fatalf("USB/network identity must not be inferred from shared name or IP: %#v", payload)
	}
}

func TestInfoBusinessErrorIsFailureEvenWithHTTP200(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": map[string]any{"code": 1, "message": "not ready", "data": map[string]any{}}})
	code, payload, _ := invoke(t, "info", "--json", d.server.URL)
	if code != 1 || len(payload["errors"].([]any)) == 0 {
		t.Fatalf("business error must fail with diagnostics: exit=%d payload=%#v", code, payload)
	}
	for _, item := range devices(t, payload) {
		if item.(map[string]any)["connected"] == true {
			t.Fatalf("error response reported as connected: %#v", item)
		}
	}
}

func TestScanNullListIsEmptyRatherThanMalformed(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/list": okData(map[string]any{"list": nil})})
	code, payload, _ := invoke(t, "scan", "--via", d.server.URL, "--no-usb", "--no-mdns", "--json")
	if code != 1 || len(devices(t, payload)) != 0 {
		t.Fatalf("USB network-mode null list should be empty: exit=%d payload=%#v", code, payload)
	}
}

func TestScanMalformedListFailsWithoutReportingDevices(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/list": okData(map[string]any{"list": "invalid"})})
	code, payload, _ := invoke(t, "scan", "--via", d.server.URL, "--no-usb", "--no-mdns", "--json")
	if code != 1 || len(devices(t, payload)) != 0 || len(payload["errors"].([]any)) == 0 {
		t.Fatalf("malformed discovery must not masquerade as empty success: exit=%d payload=%#v", code, payload)
	}
}

func TestDoctorReportsDNSAndDeviceAPIEvidence(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Diagnostic"))})
	code, payload, stderr := invoke(t, "doctor", "--json", d.server.URL)
	if code != 0 {
		t.Fatalf("doctor exit=%d stderr=%q payload=%#v", code, stderr, payload)
	}
	checks := payload["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("expected one diagnostic target, got %#v", checks)
	}
	check := checks[0].(map[string]any)
	addresses := check["resolvedAddresses"].([]any)
	if len(addresses) == 0 || addresses[0] != "127.0.0.1" || check["device"] == nil {
		t.Fatalf("doctor must distinguish resolver and HTTP evidence: %#v", check)
	}
}

func TestOfflineVersionComparisonDoesNotFetchRelease(t *testing.T) {
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Versioned"))})
	code, payload, stderr := invoke(t, "version", "--json", "--latest-version", "v1.6.0",
		"--latest-url", d.server.URL+"/must-not-fetch", d.server.URL)
	if code != 0 {
		t.Fatalf("version exit=%d stderr=%q payload=%#v", code, stderr, payload)
	}
	comparisons := payload["devices"].([]any)
	if len(comparisons) != 1 || comparisons[0].(map[string]any)["comparison"] != "outdated" {
		t.Fatalf("prerelease must compare older than stable: %#v", payload)
	}
	if d.count("/must-not-fetch") != 0 {
		t.Fatal("offline version comparison unexpectedly fetched release metadata")
	}
}

func TestVersionInvalidManifestFailsBeforeDeviceProbe(t *testing.T) {
	for _, manifest := range []any{
		map[string]any{"version": "unknown"},
		map[string]any{"version": 160},
		map[string]any{"download_url": "https://example.invalid/firmware"},
	} {
		t.Run(fmt.Sprint(manifest), func(t *testing.T) {
			d := newMockDevice(t, map[string]any{
				"/release": manifest, "/linkr/info": okData(testInfo("Must not probe")),
			})
			code, payload, _ := invoke(t, "version", "--json", "--latest-url", d.server.URL+"/release", d.server.URL)
			if code != 1 || len(payload["errors"].([]any)) == 0 || d.count("/linkr/info") != 0 {
				t.Fatalf("invalid manifest must fail without probing devices: code=%d payload=%#v", code, payload)
			}
		})
	}
}

func TestInvalidTimeoutIsArgumentError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"scan", "--timeout", "0"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("invalid timeout must exit 2, got %d stderr=%q", code, stderr.String())
	}
}

func TestWatchDoubleJSONEventsAndBoundedDuration(t *testing.T) {
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, message := range []map[string]any{
			{"event": "INFO", "data": string(mustJSON(testInfo("Observed")))},
			{"event": "CLICK", "data": `{"clicked":true}`},
			{"event": "RESET", "data": ""},
			{"event": "INFO", "data": string(mustJSON(testInfo("Observed")))},
		} {
			_, _ = fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", mustJSON(message))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	})
	d := newMockDevice(t, map[string]any{"/linkr/info": okData(testInfo("Observed")), "/linkr": stream})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run(ctx, []string{"watch", "--duration", "250ms", "--json", d.server.URL}, &stdout, &stderr)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watch duration not bounded: %s stderr=%q", elapsed, stderr.String())
	}
	if code != 0 {
		t.Fatalf("watch exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	seen := map[string]bool{}
	resetSeen, recoveredAfterReset := false, false
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("watch must output NDJSON: %v line=%q", err, line)
		}
		name, _ := event["event"].(string)
		seen[strings.ToUpper(name)] = true
		if name == "RESET" {
			device := event["device"].(map[string]any)
			if device["isResetting"] != true {
				t.Errorf("RESET should mark a pending reset: %#v", device)
			}
			resetSeen = true
		}
		if name == "INFO" && resetSeen {
			device := event["device"].(map[string]any)
			recoveredAfterReset = device["isResetting"] == false && device["connected"] == true
		}
	}
	for _, name := range []string{"INFO", "CLICK", "RESET"} {
		if !seen[name] {
			t.Errorf("missing %s event; output=%q", name, stdout.String())
		}
	}
	if !recoveredAfterReset {
		t.Errorf("fresh INFO should clear pending reset: %q", stdout.String())
	}
}

func TestWatchDisconnectInvalidatesPreviouslyFreshInfo(t *testing.T) {
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		message := map[string]any{"event": "INFO", "data": string(mustJSON(testInfo("Disconnected")))}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(message))
		w.(http.Flusher).Flush()
	})
	d := newMockDevice(t, map[string]any{"/linkr": stream})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"watch", "--duration", "150ms", "--json", d.server.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("watch exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	disconnected := false
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] != "DISCONNECTED" {
			continue
		}
		disconnected = true
		device := event["device"].(map[string]any)
		if device["connected"] != false || device["status"] != "Disconnected" ||
			device["onlineTimeSeconds"] != nil || device["heartbeat"] != nil || device["receivedAt"] != "" {
			t.Fatalf("stale INFO survived stream disconnect: %#v", device)
		}
	}
	if !disconnected {
		t.Fatalf("missing disconnection event: %q", stdout.String())
	}
}

func mustJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}
