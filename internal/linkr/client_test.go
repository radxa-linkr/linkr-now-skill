package linkr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/mdns/v2"
)

type deviceRoundTrip func(*http.Request) (*http.Response, error)

func (f deviceRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func useDeviceTransport(t *testing.T, transport deviceRoundTrip) {
	t.Helper()
	original := directClient
	directClient = &http.Client{Transport: transport}
	t.Cleanup(func() { directClient = original })
}

func validDeviceInfo() map[string]any {
	return map[string]any{"deviceName": "USB Linkr", "status": "IDLE", "onlineTime": "123.5", "heartbeat": 1234, "version": "1.6.1"}
}

func deviceAPIResponse(request *http.Request, data map[string]any) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{"code": 0, "data": data})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}, Request: request}, nil
}

func TestScanDefaultUSBFallsBackOnlyAfterUnconfirmedAlias(t *testing.T) {
	for _, failure := range []string{"none", "transport", "invalid INFO", "missing heartbeat"} {
		t.Run(failure, func(t *testing.T) {
			var requests []string
			useDeviceTransport(t, func(request *http.Request) (*http.Response, error) {
				requests = append(requests, request.URL.String())
				if deadline, ok := request.Context().Deadline(); !ok || time.Until(deadline) > time.Second {
					t.Error("USB INFO request must have a bounded deadline")
				}
				info := validDeviceInfo()
				if request.URL.Hostname() != usbFallbackIP {
					switch failure {
					case "transport":
						return nil, errors.New("alias unavailable")
					case "invalid INFO":
						info["status"] = "not a Linkr status"
					case "missing heartbeat":
						delete(info, "heartbeat")
					}
				}
				return deviceAPIResponse(request, info)
			})
			opts := DefaultOptions()
			opts.USBHost, opts.NoNetwork, opts.Timeout = "https://LINKR-USB.local.:8443", true, time.Second
			result := Scan(context.Background(), opts)
			if len(result.Devices) != 1 || len(result.Errors) != 0 {
				t.Fatalf("scan did not confirm the USB endpoint: %+v", result)
			}
			device := result.Devices[0]
			wantURL := "https://linkr-usb.local.:8443"
			wantIP := ""
			wantRequests := []string{wantURL + "/linkr/info"}
			if failure != "none" {
				wantURL, wantIP = "https://192.168.99.1:8443", usbFallbackIP
				wantRequests = append(wantRequests, wantURL+"/linkr/info")
			}
			if !slices.Equal(requests, wantRequests) {
				t.Fatalf("USB request order = %v; want %v", requests, wantRequests)
			}
			if device.Source != "usb" || device.Domain != "linkr-usb.local.:8443" || device.IP != wantIP || device.APIURL != wantURL || device.URL != wantURL || !device.Connected {
				t.Fatalf("incorrect USB identity or effective endpoint: %+v", device)
			}
		})
	}
}

func TestUSBProbeFailureKeepsBothEndpointErrors(t *testing.T) {
	useDeviceTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == usbFallbackIP {
			return nil, io.EOF
		}
		return nil, errors.New("alias unavailable")
	})
	device, err := Info(context.Background(), "linkr-usb.local", "network", time.Second)
	if err == nil || !errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "linkr-usb.local") || !strings.Contains(err.Error(), usbFallbackIP) || !strings.Contains(err.Error(), "alias unavailable") {
		t.Fatalf("both USB diagnostics must survive: device=%+v error=%v", device, err)
	}
	if device.Source != "usb" || device.Connected || device.Status != "Disconnected" || device.ReceivedAt != "" {
		t.Fatalf("an EOF response was presented as a confirmed USB device: %+v", device)
	}
}

func TestUSBOverridesAndNoUSBDoNotAddFixedAddressRequests(t *testing.T) {
	for _, host := range []string{"custom-usb.local", "192.168.99.1"} {
		t.Run(host, func(t *testing.T) {
			requests := 0
			useDeviceTransport(t, func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.Hostname() != host {
					t.Errorf("custom USB host was replaced: %s", request.URL)
				}
				return nil, errors.New("unavailable")
			})
			opts := DefaultOptions()
			opts.NoNetwork, opts.USBHost = true, host
			result := Scan(context.Background(), opts)
			if requests != 1 || len(result.Devices) != 0 || len(result.Errors) != 1 {
				t.Fatalf("custom USB origin added an extra request: %d requests, %+v", requests, result)
			}
			opts.NoUSB = true
			result = Scan(context.Background(), opts)
			if requests != 1 || len(result.Devices) != 0 || len(result.Errors) != 0 {
				t.Fatalf("--no-usb must skip both origins: %d requests, %+v", requests, result)
			}
		})
	}
}

func TestLANListedAddressFallbackKeepsExistingIdentity(t *testing.T) {
	var requests []string
	useDeviceTransport(t, func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.Host)
		if request.URL.Hostname() != "192.0.2.10" {
			return nil, errors.New("host lookup unavailable")
		}
		return deviceAPIResponse(request, validDeviceInfo())
	})
	device, err := listedDevice(map[string]any{"domain": "linkr-lan.local", "ip": "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	device, err = probe(context.Background(), device, time.Second)
	if err != nil || device.Source != "network" || device.Domain != "linkr-lan.local" || device.URL != "http://linkr-lan.local" || device.APIURL != "http://192.0.2.10" || !slices.Equal(requests, []string{"linkr-lan.local", "192.0.2.10"}) {
		t.Fatalf("LAN IP fallback changed: device=%+v requests=%v error=%v", device, requests, err)
	}
}

func TestWatchUSBFallbackClearsFreshnessAndRetriesAlias(t *testing.T) {
	initial, err := Target("linkr-usb.local", "network")
	if err != nil || initial.Source != "usb" {
		t.Fatalf("explicit default USB alias was not classified: %+v, %v", initial, err)
	}
	initial = deviceEndpoints(initial)[1]
	if err := applyInfo(&initial, validDeviceInfo()); err != nil {
		t.Fatal(err)
	}
	initial.IsResetting = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls []string
	var events []WatchEvent
	stream := func(ctx context.Context, origin string, _ time.Duration, emit func(map[string]any) error) error {
		calls = append(calls, origin)
		if len(calls) == 1 {
			err := emit(map[string]any{"event": "INFO", "data": map[string]any{"status": "invalid"}})
			if err == nil {
				t.Error("invalid USB INFO must terminate the attempt immediately")
			}
			return err
		}
		if err := emit(map[string]any{"event": "INFO", "data": validDeviceInfo()}); err != nil {
			return err
		}
		if len(calls) == 2 {
			return io.EOF
		}
		cancel()
		return ctx.Err()
	}
	watchWithStream(ctx, []Device{initial}, 100*time.Millisecond, time.Millisecond, stream, func(event WatchEvent) { events = append(events, event) })
	if !slices.Equal(calls, []string{"http://linkr-usb.local", "http://192.168.99.1", "http://linkr-usb.local"}) {
		t.Fatalf("reconnect must retry the alias before the fixed address: %v", calls)
	}
	confirmed := 0
	for _, event := range events {
		device := event.Device
		if event.Event == "CONNECTING" || event.Event == "DISCONNECTED" {
			if device.Connected || device.IsResetting || device.Heartbeat != nil || device.OnlineTimeSeconds != nil || device.ReceivedAt != "" {
				t.Fatalf("stale state crossed USB origins: %+v", event)
			}
		}
		if event.Event == "INFO" {
			confirmed++
			wantOrigin, wantIP := "http://192.168.99.1", usbFallbackIP
			if confirmed == 2 {
				wantOrigin, wantIP = "http://linkr-usb.local", ""
			}
			if device.Domain != "linkr-usb.local" || device.Source != "usb" || device.APIURL != wantOrigin || device.URL != wantOrigin || device.IP != wantIP || !device.Connected {
				t.Fatalf("incorrect confirmed USB endpoint: %+v", device)
			}
		}
	}
	if confirmed != 2 {
		t.Fatalf("expected fallback and recovered-alias INFO, got %d", confirmed)
	}
}

func TestWatchUSBRequiresTimelyValidInitialINFO(t *testing.T) {
	initial, _ := Target("linkr-usb.local", "usb")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls []string
	started := time.Now()
	stream := func(ctx context.Context, origin string, _ time.Duration, emit func(map[string]any) error) error {
		calls = append(calls, origin)
		if origin == "http://linkr-usb.local" {
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					if err := emit(map[string]any{"event": "RESET"}); err != nil {
						return err
					}
				}
			}
		}
		if time.Since(started) > 500*time.Millisecond {
			t.Error("initial USB INFO timeout waited for the 45-second stream watchdog")
		}
		if err := emit(map[string]any{"event": "INFO", "data": validDeviceInfo()}); err != nil {
			return err
		}
		// A confirmed stream must outlive the initial INFO timeout.
		select {
		case <-ctx.Done():
			t.Error("a valid initial INFO did not stop its timeout")
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
			cancel()
			return context.Canceled
		}
	}
	watchWithStream(ctx, []Device{initial}, 20*time.Millisecond, time.Second, stream, func(WatchEvent) {})
	if !slices.Equal(calls, []string{"http://linkr-usb.local", "http://192.168.99.1"}) {
		t.Fatalf("unconfirmed USB stream did not immediately fall back: %v", calls)
	}
}

func TestWatchNetworkKeepsExistingInitialINFOBehavior(t *testing.T) {
	device, _ := Target("http://192.0.2.10", "network")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var observed error
	stream := func(ctx context.Context, _ string, _ time.Duration, emit func(map[string]any) error) error {
		if err := emit(map[string]any{"event": "INFO", "data": map[string]any{"status": "invalid"}}); err != nil {
			t.Errorf("network INFO behavior changed: %v", err)
		}
		<-ctx.Done()
		observed = ctx.Err()
		return observed
	}
	watchWithStream(ctx, []Device{device}, 5*time.Millisecond, time.Second, stream, func(WatchEvent) {})
	if !errors.Is(observed, context.DeadlineExceeded) {
		t.Fatalf("network stream gained a USB initial-INFO timeout: %v", observed)
	}
}

func TestDiscoveryUsesMulticastGatewayOnlyWhenOriginalListFails(t *testing.T) {
	for _, viaFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("via fails=%v", viaFails), func(t *testing.T) {
			var badCalls, goodCalls, unusedCalls atomic.Int32
			writeList := func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{
					map[string]any{"domain": "linkr-one.local", "deviceName": "One"},
					map[string]any{"gateway": true, "domain": "linkr.local", "ip": "192.0.2.254", "port": 80},
				}}})
			}
			via := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if viaFails {
					http.Error(w, "original gateway unavailable", http.StatusServiceUnavailable)
					return
				}
				writeList(w)
			}))
			defer via.Close()
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				badCalls.Add(1)
				fmt.Fprint(w, `{"code":0,"data":{"list":"invalid"}}`)
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				goodCalls.Add(1)
				writeList(w)
			}))
			defer good.Close()
			unused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				unusedCalls.Add(1)
				writeList(w)
			}))
			defer unused.Close()
			gatewayRecord := func(server *httptest.Server) map[string]any {
				u, _ := url.Parse(server.URL)
				ip, portText, err := net.SplitHostPort(u.Host)
				if err != nil {
					t.Fatal(err)
				}
				port, _ := strconv.Atoi(portText)
				return map[string]any{"gateway": true, "domain": "linkr.local", "ip": ip, "port": port}
			}
			opts := DefaultOptions()
			opts.Via, opts.NoUSB = via.URL, true
			browse := func(context.Context) ([]map[string]any, error) {
				return []map[string]any{
					gatewayRecord(bad), gatewayRecord(good), gatewayRecord(unused),
					{"domain": "linkr-one.local", "deviceName": "One", "ip": "192.0.2.10"},
					{"domain": "linkr-two.local", "deviceName": "Two", "ip": "192.0.2.20"},
				}, nil
			}
			devices, issues := listCandidatesWithBrowser(context.Background(), opts, browse)
			if len(devices) != 2 {
				t.Fatalf("gateway leaked into physical devices or duplicated them: %+v", devices)
			}
			for _, device := range devices {
				if device.Domain == "linkr.local" || device.IP == "" {
					t.Fatalf("gateway was displayed or local metadata was lost: %+v", devices)
				}
			}
			wantCalls, wantIssues := int32(0), 0
			if viaFails {
				wantCalls, wantIssues = 1, 2
			}
			if badCalls.Load() != wantCalls || goodCalls.Load() != wantCalls || unusedCalls.Load() != 0 || len(issues) != wantIssues {
				t.Fatalf("incorrect gateway fallback: bad=%d good=%d unused=%d issues=%+v", badCalls.Load(), goodCalls.Load(), unusedCalls.Load(), issues)
			}
		})
	}
}

func TestDiscoveryMergesLocalBrowseWithIncompleteGatewayList(t *testing.T) {
	for _, gatewayFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial list", true: "unavailable gateway"}[gatewayFails], func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if gatewayFails {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{
					map[string]any{"domain": "linkr-one.local", "deviceName": "One", "ip": ""},
				}}})
			}))
			defer gateway.Close()
			opts := DefaultOptions()
			opts.Via, opts.NoUSB = gateway.URL, true
			browse := func(ctx context.Context) ([]map[string]any, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("multicast discovery needs a bounded context")
				}
				return []map[string]any{
					{"domain": "linkr-one.local", "ip": "192.0.2.10", "deviceName": "One"},
					{"domain": "linkr-two.local", "ip": "192.0.2.20", "deviceName": "Two"},
				}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			devices, issues := listCandidatesWithBrowser(ctx, opts, browse)
			if len(devices) != 2 || devices[0].Domain != "linkr-one.local" || devices[0].IP != "192.0.2.10" || devices[1].Domain != "linkr-two.local" {
				t.Fatalf("discovery lost or duplicated a device: %+v", devices)
			}
			if len(issues) != map[bool]int{false: 0, true: 1}[gatewayFails] {
				t.Fatalf("unexpected gateway diagnostics: %+v", issues)
			}
		})
	}
}

func TestMDNSDiscoveryPreservesNonDefaultHTTPPort(t *testing.T) {
	item, ok := mdnsDevice(mdns.ServiceEvent{
		Addr: netip.MustParseAddr("192.0.2.10"),
		Instance: mdns.ServiceInstance{
			Instance: "Desk", Service: "_http._tcp", Domain: "local",
			Host: "linkr-one.local.", Port: 8080,
			Text: []mdns.TXTEntry{mdns.NewTXTString("id", "linkr"), mdns.NewTXTString("device_name", "Desk")},
		},
	})
	if !ok {
		t.Fatal("Linkr service was not recognized")
	}
	d, err := listedDevice(item)
	if err != nil || d.APIURL != "http://linkr-one.local:8080" || d.IP != "192.0.2.10" {
		t.Fatalf("lost mDNS origin: %+v, error=%v", d, err)
	}
}
