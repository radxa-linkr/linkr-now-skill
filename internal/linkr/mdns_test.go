package linkr

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/pion/mdns/v2"
)

func discoveryEvent(instance, host, address string, text ...mdns.TXTEntry) mdns.ServiceEvent {
	return mdns.ServiceEvent{
		Instance: mdns.ServiceInstance{Instance: instance, Host: host, Service: "_http._tcp", Domain: "local", Port: 8080, Text: text},
		Addr:     netip.MustParseAddr(address),
	}
}

func TestMDNSDeviceUsesResolvedServiceMetadata(t *testing.T) {
	event := discoveryEvent("Desk", "LINKR-One.local.", "192.0.2.10",
		mdns.NewTXTString("id", "linkr"),
		mdns.NewTXTString("DEVICE_NAME", "Desk computer"),
		mdns.NewTXTString("os_version", "v1.6.0"),
	)
	device, ok := mdnsDevice(event)
	expected := map[string]any{"domain": "linkr-one.local", "deviceName": "Desk computer", "ip": "192.0.2.10", "version": "v1.6.0", "port": 8080}
	if !ok || !reflect.DeepEqual(device, expected) {
		t.Fatalf("device=%v accepted=%v", device, ok)
	}
}

func TestMDNSDeviceAcceptsLinkrInstanceWithoutID(t *testing.T) {
	event := discoveryEvent("linkr", "linkr-usb.local", "169.254.1.1")
	device, ok := mdnsDevice(event)
	if !ok || device["ip"] != "169.254.1.1" {
		t.Fatalf("USB link-local service rejected: %v", device)
	}
}

func TestMDNSDeviceRejectsUnrelatedAndUnusableServices(t *testing.T) {
	valid := func() mdns.ServiceEvent {
		return discoveryEvent("desk", "linkr-one.local", "192.0.2.10", mdns.NewTXTString("id", "linkr"))
	}
	cases := map[string]func(*mdns.ServiceEvent){
		"unrelated": func(event *mdns.ServiceEvent) { event.Instance.Text = nil },
		"gateway":   func(event *mdns.ServiceEvent) { event.Instance.Host = "LINKR.LOCAL." },
		"loopback":  func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("127.0.0.1") },
		"unspecified": func(event *mdns.ServiceEvent) {
			event.Addr = netip.MustParseAddr("0.0.0.0")
		},
		"multicast": func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("224.0.0.251") },
		"IPv6 only": func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("2001:db8::1") },
		"missing address": func(event *mdns.ServiceEvent) {
			event.Addr = netip.Addr{}
		},
		"wrong service": func(event *mdns.ServiceEvent) { event.Instance.Service = "_printer._tcp" },
		"wrong domain":  func(event *mdns.ServiceEvent) { event.Instance.Domain = "example.com" },
		"invalid host":  func(event *mdns.ServiceEvent) { event.Instance.Host = "bad name.local" },
		"missing port":  func(event *mdns.ServiceEvent) { event.Instance.Port = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := valid()
			mutate(&event)
			if device, ok := mdnsDevice(event); ok {
				t.Fatalf("accepted unusable service: %v", device)
			}
		})
	}
}

func TestMDNSDiscoveryDeduplicatesDomainsAndSorts(t *testing.T) {
	byDomain := make(map[string]map[string]any)
	for _, event := range []mdns.ServiceEvent{
		discoveryEvent("linkr", "linkr-b.local", "192.0.2.20"),
		discoveryEvent("linkr", "linkr-a.local", "192.0.2.10"),
		discoveryEvent("linkr", "LINKR-A.LOCAL.", "192.0.2.11"),
	} {
		device, ok := mdnsDevice(event)
		if !ok {
			t.Fatal("rejected Linkr fixture")
		}
		byDomain[device["domain"].(string)] = device
	}
	devices := sortedMDNSRecords(byDomain)
	if len(devices) != 2 || devices[0]["domain"] != "linkr-a.local" || devices[0]["ip"] != "192.0.2.11" || devices[1]["domain"] != "linkr-b.local" {
		t.Fatalf("unexpected discoveries: %v", devices)
	}
}

func gatewayEvent(address string, port uint16) mdns.ServiceEvent {
	return mdns.ServiceEvent{
		Instance: mdns.ServiceInstance{Instance: "linkr", Host: "linkr.local", Service: "_linkr._tcp", Domain: "local", Port: port},
		Addr:     netip.MustParseAddr(address),
	}
}

func TestMDNSGatewayAcceptsEmptyTXTAndKeepsEndpoint(t *testing.T) {
	event := gatewayEvent("192.0.2.10", 80)
	event.Instance.Instance, event.Instance.Host, event.Instance.Domain = "LINKR", "LINKR.LOCAL.", "LOCAL."
	gateway, ok := mdnsGateway(event)
	expected := map[string]any{"gateway": true, "domain": "linkr.local", "ip": "192.0.2.10", "port": 80}
	if !ok || !reflect.DeepEqual(gateway, expected) {
		t.Fatalf("gateway=%v accepted=%v", gateway, ok)
	}
	if device, ok := mdnsDevice(event); ok {
		t.Fatalf("gateway was exposed as a device: %v", device)
	}
	event = gatewayEvent("::ffff:192.0.2.10", 8080)
	if gateway, ok := mdnsGateway(event); !ok || gateway["ip"] != "192.0.2.10" || gateway["port"] != 8080 {
		t.Fatalf("gateway lost its mapped IPv4 or SRV port: %v", gateway)
	}
}

func TestMDNSGatewayRejectsUnrelatedAndUnusableServices(t *testing.T) {
	cases := map[string]func(*mdns.ServiceEvent){
		"HTTP device service": func(event *mdns.ServiceEvent) { event.Instance.Service = "_http._tcp" },
		"wrong service":       func(event *mdns.ServiceEvent) { event.Instance.Service = "_printer._tcp" },
		"wrong instance":      func(event *mdns.ServiceEvent) { event.Instance.Instance = "desk" },
		"wrong host":          func(event *mdns.ServiceEvent) { event.Instance.Host = "linkr-one.local" },
		"wrong domain":        func(event *mdns.ServiceEvent) { event.Instance.Domain = "example.com" },
		"missing port":        func(event *mdns.ServiceEvent) { event.Instance.Port = 0 },
		"loopback":            func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("127.0.0.1") },
		"unspecified":         func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("0.0.0.0") },
		"multicast":           func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("224.0.0.251") },
		"broadcast":           func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("255.255.255.255") },
		"IPv6 only":           func(event *mdns.ServiceEvent) { event.Addr = netip.MustParseAddr("2001:db8::1") },
		"missing address":     func(event *mdns.ServiceEvent) { event.Addr = netip.Addr{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := gatewayEvent("192.0.2.10", 80)
			mutate(&event)
			if gateway, ok := mdnsGateway(event); ok {
				t.Fatalf("accepted unusable discovery entry point: %v", gateway)
			}
		})
	}
	device := discoveryEvent("linkr", "linkr-one.local", "192.0.2.10")
	if gateway, ok := mdnsGateway(device); ok {
		t.Fatalf("HTTP device was treated as an entry point: %v", gateway)
	}
}

type mdnsBrowseFixture struct {
	ctx      context.Context
	cancel   context.CancelFunc
	callback func(mdns.ServiceEvent)
	services []string
	events   map[string][]mdns.ServiceEvent
}

func (fixture *mdnsBrowseFixture) OnServiceDiscovered(callback func(mdns.ServiceEvent)) {
	fixture.callback = callback
}

func (fixture *mdnsBrowseFixture) Browse(ctx context.Context, service string) error {
	if ctx != fixture.ctx {
		return errors.New("browse sessions did not share their context")
	}
	fixture.services = append(fixture.services, service)
	for _, event := range fixture.events[service] {
		fixture.callback(event)
	}
	if len(fixture.services) == 2 {
		fixture.cancel()
	}
	return nil
}

func TestMDNSBrowseMergesDevicesAndDistinctGatewayEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &mdnsBrowseFixture{ctx: ctx, cancel: cancel, events: map[string][]mdns.ServiceEvent{
		mdnsService: {
			discoveryEvent("linkr", "linkr-one.local", "192.0.2.10"),
			discoveryEvent("linkr", "LINKR-ONE.LOCAL.", "192.0.2.11"),
			// An HTTP advertisement for the elected alias remains excluded.
			discoveryEvent("linkr", "linkr.local", "192.0.2.20"),
		},
		mdnsGatewayService: {
			gatewayEvent("192.0.2.20", 80),
			gatewayEvent("192.0.2.20", 80),
			gatewayEvent("192.0.2.20", 8080),
			gatewayEvent("192.0.2.21", 80),
		},
	}}
	records, err := browseMDNSServices(ctx, fixture)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("explicit cancellation was lost: %v", err)
	}
	if !reflect.DeepEqual(fixture.services, []string{mdnsService, mdnsGatewayService}) {
		t.Fatalf("wrong browse sessions: %v", fixture.services)
	}
	if len(records) != 4 || records[0]["domain"] != "linkr-one.local" || records[0]["ip"] != "192.0.2.11" || records[0]["gateway"] != nil {
		t.Fatalf("wrong device merge: %v", records)
	}
	for index, endpoint := range []struct {
		ip   string
		port int
	}{{"192.0.2.20", 80}, {"192.0.2.20", 8080}, {"192.0.2.21", 80}} {
		record := records[index+1]
		if record["gateway"] != true || record["domain"] != "linkr.local" || record["ip"] != endpoint.ip || record["port"] != endpoint.port {
			t.Fatalf("lost distinct entry point: %v", records)
		}
	}
}

func TestMDNSInterfaceSelectionIncludesLANAndUSB(t *testing.T) {
	active := net.FlagUp | net.FlagMulticast
	candidates := []net.Interface{
		{Index: 1, Name: "loopback", Flags: active | net.FlagLoopback, MTU: 65536},
		{Index: 2, Name: "vpn", Flags: net.FlagUp | net.FlagPointToPoint, MTU: 1500},
		{Index: 3, Name: "LAN", Flags: active, MTU: 1500},
		{Index: 4, Name: "USB", Flags: active, MTU: 1500},
		{Index: 5, Name: "down", Flags: net.FlagMulticast, MTU: 1500},
		{Index: 6, Name: "IPv6-only", Flags: active, MTU: 1500},
		{Index: 7, Name: "address-error", Flags: active, MTU: 1500},
	}
	addresses := func(iface net.Interface) ([]net.Addr, error) {
		switch iface.Name {
		case "LAN":
			return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.2")}}, nil
		case "USB":
			return []net.Addr{&net.IPAddr{IP: net.ParseIP("169.254.1.2")}}, nil
		case "IPv6-only":
			return []net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::1")}}, nil
		case "address-error":
			return nil, errors.New("adapter disconnected")
		default:
			t.Fatalf("attempted to enumerate an unsuitable interface: %s", iface.Name)
			return nil, nil
		}
	}
	selected := selectMDNSInterfaces(candidates, addresses)
	if len(selected) != 2 || selected[0].Name != "LAN" || selected[1].Name != "USB" {
		t.Fatalf("selected wrong interfaces: %v", selected)
	}
}

func TestMDNSRejectsInvalidHostsAndHonorsCancellationBeforeNetworking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, host := range []string{"example.com", "http://one.local", "bad name.local", "../one.local"} {
		if _, err := ResolveMDNS(ctx, host); err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("did not reject invalid host %q: %v", host, err)
		}
	}
	if _, err := ResolveMDNS(ctx, "one.local"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve cancellation: %v", err)
	}
	if _, err := BrowseMDNS(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("browse cancellation: %v", err)
	}
}
