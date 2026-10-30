package linkr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/mdns/v2"
	"golang.org/x/net/ipv4"
)

const (
	mdnsService        = "_http._tcp"
	mdnsGatewayService = "_linkr._tcp"
)

func validMDNSHost(host string) bool {
	if !strings.HasSuffix(host, ".local") || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
				return false
			}
		}
	}
	return true
}

// selectMDNSInterfaces keeps LAN and USB adapters independent of the default
// route, which can point at a VPN or a proxy's virtual adapter.
func selectMDNSInterfaces(candidates []net.Interface, addresses func(net.Interface) ([]net.Addr, error)) []net.Interface {
	selected := make([]net.Interface, 0, len(candidates))
	for _, iface := range candidates {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 || iface.MTU <= 28 {
			continue
		}
		addrs, err := addresses(iface)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() {
				selected = append(selected, iface)
				break
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Index < selected[j].Index })
	return selected
}

func newMDNSClient(ctx context.Context) (*mdns.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate mDNS interfaces: %w", err)
	}
	interfaces = selectMDNSInterfaces(interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
	if len(interfaces) == 0 {
		return nil, errors.New("no active multicast-capable IPv4 network interface")
	}
	// A multicast bind enables shared UDP-port reuse on the supported platforms.
	// Pion joins 224.0.0.251 on the explicitly selected interfaces and selects
	// each outgoing multicast interface itself. Binding its recommended .0
	// address here avoids joining an arbitrary default interface first.
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(224, 0, 0, 0), Port: 5353})
	if err != nil {
		return nil, fmt.Errorf("open shared mDNS socket: %w", err)
	}
	logger := &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}
	client, err := mdns.NewServer(ipv4.NewPacketConn(socket), nil,
		mdns.WithInterfaces(interfaces...),
		mdns.WithLoggerFactory(logger),
		mdns.WithQueryInterval(time.Second),
		mdns.WithCacheRefresh(false),
	)
	if err != nil {
		_ = socket.Close()
		return nil, fmt.Errorf("start mDNS client: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// ResolveMDNS resolves a .local host through each active IPv4 multicast
// interface, including USB adapters, without using the default multicast route.
func ResolveMDNS(ctx context.Context, host string) ([]string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if !validMDNSHost(host) {
		return nil, errors.New("mDNS requires a valid .local hostname")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	client, err := newMDNSClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	_, addr, err := client.QueryAddr(ctx, host)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return []string{}, nil
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("resolve mDNS hostname: %w", err)
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLoopback() {
		return nil, errors.New("mDNS hostname has no usable IPv4 address")
	}
	return []string{addr.String()}, nil
}

// mdnsDevice maps a resolved public DNS-SD event to the discovery-list contract.
func mdnsDevice(event mdns.ServiceEvent) (map[string]any, bool) {
	instance := event.Instance
	host := strings.ToLower(strings.TrimSuffix(instance.Host, "."))
	if !strings.EqualFold(instance.Service, mdnsService) || !strings.EqualFold(strings.TrimSuffix(instance.Domain, "."), "local") || host == "linkr.local" || !validMDNSHost(host) || instance.Port == 0 {
		return nil, false
	}
	ip := event.Addr.Unmap()
	if !ip.IsValid() || !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, false
	}
	identified := strings.EqualFold(instance.Instance, "linkr")
	name, version := "linkr", ""
	for _, entry := range instance.Text {
		switch strings.ToLower(entry.Key) {
		case "id":
			identified = identified || string(entry.Value) == "linkr"
		case "device_name":
			name = string(entry.Value)
		case "os_version":
			version = string(entry.Value)
		}
	}
	if !identified {
		return nil, false
	}
	return map[string]any{"domain": host, "deviceName": name, "ip": ip.String(), "version": version, "port": int(instance.Port)}, true
}

// mdnsGateway identifies a LAN discovery entry point, which is not itself a
// device record. Unlike HTTP device advertisements, its TXT record may be empty.
func mdnsGateway(event mdns.ServiceEvent) (map[string]any, bool) {
	instance := event.Instance
	host := strings.ToLower(strings.TrimSuffix(instance.Host, "."))
	if !strings.EqualFold(instance.Service, mdnsGatewayService) ||
		!strings.EqualFold(strings.TrimSuffix(instance.Domain, "."), "local") ||
		!strings.EqualFold(instance.Instance, "linkr") || host != "linkr.local" || instance.Port == 0 {
		return nil, false
	}
	ip := event.Addr.Unmap()
	if !ip.IsValid() || !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.As4() == [4]byte{255, 255, 255, 255} {
		return nil, false
	}
	return map[string]any{"gateway": true, "domain": host, "ip": ip.String(), "port": int(instance.Port)}, true
}

func sortedMDNSRecords(byKey map[string]map[string]any) []map[string]any {
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		records = append(records, byKey[key])
	}
	return records
}

type mdnsServiceBrowser interface {
	OnServiceDiscovered(func(mdns.ServiceEvent))
	Browse(context.Context, string) error
}

func browseMDNSServices(ctx context.Context, client mdnsServiceBrowser) ([]map[string]any, error) {
	var mu sync.Mutex
	byDomain := make(map[string]map[string]any)
	byGatewayEndpoint := make(map[string]map[string]any)
	client.OnServiceDiscovered(func(event mdns.ServiceEvent) {
		mu.Lock()
		defer mu.Unlock()
		if device, ok := mdnsDevice(event); ok {
			byDomain[device["domain"].(string)] = device
		} else if gateway, ok := mdnsGateway(event); ok {
			// Several adapters may announce the same elected name at different
			// addresses. Retain every entry point, while merging repeats.
			key := net.JoinHostPort(gateway["ip"].(string), fmt.Sprint(gateway["port"]))
			byGatewayEndpoint[key] = gateway
		}
	})
	// Pion Browse starts an asynchronous session. Both sessions share this
	// client, callback, and deadline, rather than extending the browse window.
	for _, service := range []string{mdnsService, mdnsGatewayService} {
		if err := client.Browse(ctx, service); err != nil {
			return nil, fmt.Errorf("browse mDNS service %s: %w", service, err)
		}
	}
	<-ctx.Done()
	mu.Lock()
	records := append(sortedMDNSRecords(byDomain), sortedMDNSRecords(byGatewayEndpoint)...)
	mu.Unlock()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return records, nil
	}
	return records, ctx.Err()
}

// BrowseMDNS observes HTTP devices and LAN discovery entry points until the
// context's deadline. Records marked gateway=true are entry points to query,
// not devices to display. This client advertises no services.
func BrowseMDNS(ctx context.Context) ([]map[string]any, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	client, err := newMDNSClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return browseMDNSServices(ctx, client)
}
