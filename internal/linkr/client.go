package linkr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device keeps transport freshness separate from the device's KVM status.
// A USB and a LAN entry may refer to the same physical device.
type Device struct {
	Source            string   `json:"source"`
	Domain            string   `json:"domain"`
	Name              string   `json:"name"`
	IP                string   `json:"ip"`
	URL               string   `json:"url"`
	APIURL            string   `json:"apiUrl"`
	Status            string   `json:"status"`
	Connected         bool     `json:"connected"`
	OnlineTimeSeconds *float64 `json:"onlineTimeSeconds"`
	Heartbeat         *int64   `json:"heartbeat"`
	ReceivedAt        string   `json:"receivedAt"`
	Version           string   `json:"version"`
	IsResetting       bool     `json:"isResetting"`
}

type Issue struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	Message string `json:"message"`
}

type Result struct {
	Devices []Device `json:"devices"`
	Errors  []Issue  `json:"errors"`
}

type Options struct {
	Via       string
	USBHost   string
	NoUSB     bool
	NoNetwork bool
	NoMDNS    bool
	Targets   []string
	Timeout   time.Duration
}

func DefaultOptions() Options {
	return Options{Via: "linkr.local", USBHost: "linkr-usb.local", Timeout: 10 * time.Second}
}

const usbFallbackIP = "192.168.99.1"

func text(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func number(v any) (float64, bool) {
	var n float64
	var err error
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case json.Number:
		n, err = x.Float64()
	case string:
		n, err = strconv.ParseFloat(x, 64)
	default:
		return 0, false
	}
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
}

func newDevice(host, source string, item map[string]any) (Device, error) {
	base, err := BaseURL(host)
	if err != nil {
		return Device{}, err
	}
	u, _ := url.Parse(base)
	d := Device{Source: source, Domain: u.Host, URL: base, APIURL: base,
		Status: "Connecting", Name: text(item, "deviceName"), IP: text(item, "ip"), Version: text(item, "version")}
	if d.Name == "" {
		d.Name = strings.TrimSuffix(strings.TrimPrefix(u.Hostname(), "linkr-"), ".local")
	}
	if net.ParseIP(u.Hostname()) != nil {
		d.IP = u.Hostname()
	}
	return d, nil
}

func Target(host, source string) (Device, error) {
	d, err := newDevice(host, source, nil)
	if err == nil {
		u, _ := url.Parse(d.URL)
		if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "linkr-usb.local") {
			d.Source = "usb"
		}
	}
	return d, err
}

// deviceEndpoints gives the default USB alias a fixed-address fallback. Derive
// the alias from Domain so each reconnect retries it after an earlier fallback.
func deviceEndpoints(d Device) []Device {
	if d.Source != "usb" {
		return []Device{d}
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		return []Device{d}
	}
	u.Host = d.Domain
	if !strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "linkr-usb.local") {
		return []Device{d}
	}
	alias := d
	alias.IP, alias.URL, alias.APIURL = "", u.String(), u.String()
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(usbFallbackIP, port)
	} else {
		u.Host = usbFallbackIP
	}
	fallback := d
	fallback.IP, fallback.URL, fallback.APIURL = usbFallbackIP, u.String(), u.String()
	return []Device{alias, fallback}
}

func listedDevice(item map[string]any) (Device, error) {
	host := text(item, "domain")
	if port, ok := number(item["port"]); ok && port > 0 && port != 80 {
		if port > 65535 || math.Trunc(port) != port {
			return Device{}, fmt.Errorf("invalid service port")
		}
		host = net.JoinHostPort(strings.TrimSuffix(host, "."), strconv.Itoa(int(port)))
	}
	return newDevice(host, "network", item)
}

func applyInfo(d *Device, info map[string]any) error {
	status := text(info, "status")
	if status != "UNCONFIGURED" && status != "IDLE" && status != "USING" {
		return fmt.Errorf("invalid Linkr status %q", status)
	}
	uptime, ok := number(info["onlineTime"])
	if !ok {
		return fmt.Errorf("invalid Linkr onlineTime")
	}
	heartbeat, ok := number(info["heartbeat"])
	if !ok || heartbeat >= float64(math.MaxInt64) || math.Trunc(heartbeat) != heartbeat {
		return fmt.Errorf("invalid Linkr heartbeat")
	}
	h := int64(heartbeat)
	d.Status, d.Connected, d.IsResetting = status, true, false
	d.OnlineTimeSeconds, d.Heartbeat = &uptime, &h
	d.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if name := text(info, "deviceName"); name != "" {
		d.Name = name
	}
	d.Version = text(info, "version")
	return nil
}

func Info(ctx context.Context, host, source string, timeout time.Duration) (Device, error) {
	d, err := Target(host, source)
	if err != nil {
		return d, err
	}
	return probe(ctx, d, timeout)
}

func probe(ctx context.Context, d Device, timeout time.Duration) (Device, error) {
	var err error
	var attemptErrors []error
	for _, endpoint := range deviceEndpoints(d) {
		if ctx.Err() != nil {
			d.Status, d.Connected = "Disconnected", false
			return d, ctx.Err()
		}
		d, err = probeEndpoint(ctx, endpoint, timeout)
		if err == nil {
			return d, nil
		}
		attemptErrors = append(attemptErrors, fmt.Errorf("%s: %w", endpoint.APIURL, err))
	}
	if len(attemptErrors) > 1 {
		err = errors.Join(attemptErrors...)
	}
	return d, err
}

func probeEndpoint(ctx context.Context, d Device, timeout time.Duration) (Device, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	info, err := APIData(requestCtx, d.APIURL, "/linkr/info")
	if err != nil && d.IP != "" {
		u, _ := url.Parse(d.APIURL)
		if u.Hostname() != d.IP && net.ParseIP(d.IP) != nil && ctx.Err() == nil {
			// The list can supply an address even when the host lacks mDNS support.
			if port := u.Port(); port != "" {
				u.Host = net.JoinHostPort(d.IP, port)
			} else if strings.Contains(d.IP, ":") {
				u.Host = "[" + d.IP + "]"
			} else {
				u.Host = d.IP
			}
			fallbackCtx, fallbackCancel := context.WithTimeout(ctx, timeout)
			info, err = APIData(fallbackCtx, u.String(), "/linkr/info")
			fallbackCancel()
			if err == nil {
				d.APIURL = u.String()
			}
		}
	}
	if err == nil {
		err = applyInfo(&d, info)
	}
	if err != nil {
		d.Status, d.Connected = "Disconnected", false
	}
	return d, err
}

func listCandidates(ctx context.Context, opts Options) ([]Device, []Issue) {
	return listCandidatesWithBrowser(ctx, opts, BrowseMDNS)
}

func requestDeviceList(ctx context.Context, host string, timeout time.Duration) ([]any, error) {
	base, err := BaseURL(host)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := APIData(requestCtx, base, "/linkr/list")
	if err != nil || data["list"] == nil {
		return nil, err
	}
	entries, ok := data["list"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid data.list: expected array or null")
	}
	return entries, nil
}

func gatewayOrigin(item map[string]any) (string, error) {
	ip := text(item, "ip")
	port, ok := number(item["port"])
	if net.ParseIP(ip) == nil || !ok || port < 1 || port > 65535 || math.Trunc(port) != port {
		return "", fmt.Errorf("invalid multicast gateway address or port")
	}
	return BaseURL(net.JoinHostPort(ip, strconv.Itoa(int(port))))
}

func listCandidatesWithBrowser(ctx context.Context, opts Options, browse func(context.Context) ([]map[string]any, error)) ([]Device, []Issue) {
	candidates := []Device{}
	issues := []Issue{}
	if !opts.NoNetwork {
		type browseResult struct {
			items []map[string]any
			err   error
		}
		var discovered chan browseResult
		if !opts.NoMDNS {
			discovered = make(chan browseResult, 1)
			go func() {
				browseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				items, err := browse(browseCtx)
				discovered <- browseResult{items, err}
			}()
		}
		entries, err := requestDeviceList(ctx, opts.Via, opts.Timeout)
		if err != nil {
			issues = append(issues, Issue{"network", opts.Via, err.Error()})
		}
		// A reachable entry point may return only part of the LAN. Merge the
		// independent local browse even when the HTTP list request succeeds.
		if discovered != nil {
			found := <-discovered
			if found.err != nil {
				issues = append(issues, Issue{"mdns", "_http._tcp.local", found.err.Error()})
			}
			gateways := []map[string]any{}
			for _, item := range found.items {
				if item["gateway"] == true {
					gateways = append(gateways, item)
				} else {
					entries = append(entries, item)
				}
			}
			if err != nil {
				for _, gateway := range gateways {
					if ctx.Err() != nil {
						break
					}
					origin, gatewayErr := gatewayOrigin(gateway)
					var listed []any
					if gatewayErr == nil {
						listed, gatewayErr = requestDeviceList(ctx, origin, opts.Timeout)
					}
					if gatewayErr == nil {
						entries = append(entries, listed...)
						break
					}
					issues = append(issues, Issue{"network", text(gateway, "domain"), gatewayErr.Error()})
				}
			}
		}
		for _, entry := range entries {
			item, ok := entry.(map[string]any)
			if ok && item["gateway"] == true {
				continue
			}
			if !ok || text(item, "domain") == "" {
				issues = append(issues, Issue{"network", opts.Via, "list item is missing a domain"})
				continue
			}
			d, itemErr := listedDevice(item)
			if itemErr != nil {
				issues = append(issues, Issue{"network", text(item, "domain"), itemErr.Error()})
				continue
			}
			candidates = append(candidates, d)
		}
	}
	for _, target := range opts.Targets {
		d, err := newDevice(target, "network", nil)
		if err != nil {
			issues = append(issues, Issue{"network", target, err.Error()})
		} else {
			candidates = append(candidates, d)
		}
	}
	seen := map[string]int{}
	unique := []Device{}
	for _, d := range candidates {
		key := strings.ToLower(strings.TrimSuffix(d.Domain, "."))
		if index, exists := seen[key]; exists {
			if unique[index].IP == "" {
				unique[index].IP = d.IP
			}
		} else {
			seen[key] = len(unique)
			unique = append(unique, d)
		}
	}
	return unique, issues
}

func Scan(ctx context.Context, opts Options) Result {
	result := Result{Devices: []Device{}, Errors: []Issue{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	add := func(d Device, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.Errors = append(result.Errors, Issue{d.Source, d.Domain, err.Error()})
			if d.Source == "usb" {
				return // USB card disappears when its transport is absent.
			}
		}
		result.Devices = append(result.Devices, d)
	}
	if !opts.NoUSB {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Info(ctx, opts.USBHost, "usb", opts.Timeout)
			if err != nil && d.Domain == "" {
				d.Source, d.Domain = "usb", opts.USBHost
			}
			add(d, err)
		}()
	}
	candidates, issues := listCandidates(ctx, opts)
	mu.Lock()
	result.Errors = append(result.Errors, issues...)
	mu.Unlock()
	limit := make(chan struct{}, 4)
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				add(candidate, ctx.Err())
				return
			}
			d, err := probe(ctx, candidate, opts.Timeout)
			add(d, err)
		}()
	}
	wg.Wait()
	sort.Slice(result.Devices, func(i, j int) bool {
		a, b := result.Devices[i], result.Devices[j]
		if a.Source != b.Source {
			return a.Source == "usb"
		}
		return a.Domain < b.Domain
	})
	sort.Slice(result.Errors, func(i, j int) bool { return result.Errors[i].Target < result.Errors[j].Target })
	return result
}

type WatchEvent struct {
	Event     string  `json:"event"`
	Device    *Device `json:"device,omitempty"`
	Message   string  `json:"message,omitempty"`
	Timestamp string  `json:"timestamp"`
}

// Watch reconnects after five seconds and discards stale status on each attempt.
// CLICK and RESET only observe physical events; they do not send any commands.
func Watch(ctx context.Context, devices []Device, timeout time.Duration, emit func(WatchEvent)) {
	watchWithStream(ctx, devices, timeout, 5*time.Second, StreamEventsWithTimeout, emit)
}

func watchWithStream(ctx context.Context, devices []Device, timeout, reconnectDelay time.Duration,
	stream func(context.Context, string, time.Duration, func(map[string]any) error) error,
	emit func(WatchEvent),
) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	send := func(event string, d Device, message string) {
		mu.Lock()
		defer mu.Unlock()
		emit(WatchEvent{event, &d, message, time.Now().UTC().Format(time.RFC3339Nano)})
	}
	for _, initial := range devices {
		initial := initial
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := initial
			for ctx.Err() == nil {
				for _, endpoint := range deviceEndpoints(d) {
					if ctx.Err() != nil {
						return
					}
					d = endpoint
					d.Connected, d.IsResetting, d.Status = false, false, "Connecting"
					d.OnlineTimeSeconds, d.Heartbeat, d.ReceivedAt = nil, nil, ""
					send("CONNECTING", d, "")
					attemptCtx, cancelAttempt := context.WithCancelCause(ctx)
					var initialInfoTimer *time.Timer
					if d.Source == "usb" {
						initialInfoTimer = time.AfterFunc(timeout, func() {
							cancelAttempt(fmt.Errorf("no valid Linkr INFO within %s", timeout))
						})
					}
					confirmed := false
					err := stream(attemptCtx, d.APIURL, timeout, func(msg map[string]any) error {
						event := text(msg, "event")
						var data map[string]any
						switch value := msg["data"].(type) {
						case map[string]any:
							data = value
						case string:
							if value != "" {
								if err := json.Unmarshal([]byte(value), &data); err != nil {
									message := "invalid nested SSE JSON: " + err.Error()
									send("ERROR", d, message)
									if d.Source == "usb" && event == "INFO" {
										return fmt.Errorf("%s", message)
									}
									return nil
								}
							}
						}
						switch event {
						case "INFO":
							if err := applyInfo(&d, data); err != nil {
								send("ERROR", d, err.Error())
								if d.Source == "usb" {
									return err
								}
								return nil
							}
							confirmed = true
							if initialInfoTimer != nil {
								initialInfoTimer.Stop()
							}
						case "CLICK":
							if data["clicked"] != true {
								return nil
							}
							d.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
						case "RESET":
							d.IsResetting = true
							d.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
						default:
							return nil
						}
						send(event, d, "")
						return nil
					})
					if initialInfoTimer != nil {
						initialInfoTimer.Stop()
					}
					cause := context.Cause(attemptCtx)
					cancelAttempt(nil)
					if ctx.Err() != nil {
						return
					}
					if cause != nil {
						err = cause
					}
					d.Connected, d.IsResetting, d.Status = false, false, "Disconnected"
					d.OnlineTimeSeconds, d.Heartbeat, d.ReceivedAt = nil, nil, ""
					message := "SSE closed"
					if err != nil {
						message = err.Error()
					}
					send("DISCONNECTED", d, message)
					if confirmed {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(reconnectDelay):
				}
			}
		}()
	}
	wg.Wait()
}
