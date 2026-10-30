package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"linkr-now-skill/internal/linkr"
)

var buildVersion = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `Linkr Now: discover and monitor devices without a browser or runtime.

Usage: linkr-now <command> [options] [hosts...]
  scan       Discover USB and LAN devices and fetch current status
  info HOST  Query one device by hostname, IP, or HTTP(S) origin
  watch      Listen for INFO / CLICK / RESET events (default: 60 seconds)
  doctor     Diagnose DNS, mDNS, and the device HTTP API
  version    Fetch release information; optionally compare device versions

Common options:
  --json             JSON output; watch uses one JSON object per line
  --timeout 10s      Per-request timeout (bare seconds also accepted)
Discovery options (scan / watch / version):
  --via HOST         LAN discovery gateway (default: linkr.local)
  --usb-host HOST    USB network endpoint (default: linkr-usb.local)
  --no-usb           Skip USB discovery
  --no-network       Skip gateway and multicast discovery
  --no-mdns          Disable local multicast browsing
  --target HOST      Additional target (repeatable)
watch options:
  --duration 60s     Monitoring time, including initial discovery
version options:
  --latest-version V Compare against a supplied version, without Internet access
  --latest-url URL   Override the official release metadata URL

Examples:
  linkr-now scan --json
  linkr-now scan --via 192.0.2.10 --no-usb
  linkr-now info 192.0.2.10 --json
  linkr-now watch --duration 45s 192.0.2.10 --json
  linkr-now version --latest-version v1.6.0 192.0.2.10 --json
`

type durationValue struct{ value *time.Duration }

func (d durationValue) String() string { return d.value.String() }
func (d durationValue) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		seconds, parseErr := strconv.ParseFloat(s, 64)
		if parseErr != nil || seconds <= 0 || seconds > 86400 {
			return fmt.Errorf("expected a positive duration up to 24h, e.g. 10s")
		}
		v = time.Duration(seconds * float64(time.Second))
	}
	if v <= 0 || v > 24*time.Hour {
		return fmt.Errorf("duration must be between 0 and 24h")
	}
	*d.value = v
	return nil
}

type stringsValue []string

func (s *stringsValue) String() string { return strings.Join(*s, ", ") }
func (s *stringsValue) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Accept both 'info --json HOST' and 'info HOST --json' on all platforms.
func parse(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil || hasValue {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return fs.Parse(append(append(flags, "--"), positional...))
}

func encode(w io.Writer, value any) {
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	_ = e.Encode(value)
}

func printResult(w, stderr io.Writer, result linkr.Result, jsonMode bool) int {
	if jsonMode {
		encode(w, result)
	} else {
		t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(t, "SOURCE\tNAME\tSTATUS\tVERSION\tUPTIME(s)\tADDRESS\tAPI")
		for _, d := range result.Devices {
			uptime := "-"
			if d.OnlineTimeSeconds != nil {
				uptime = strconv.FormatFloat(*d.OnlineTimeSeconds, 'f', 2, 64)
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Source, safe(d.Name), d.Status, safe(d.Version), uptime, d.URL, d.APIURL)
		}
		_ = t.Flush()
		for _, issue := range result.Errors {
			fmt.Fprintf(stderr, "%s (%s): %s\n", safe(issue.Target), issue.Source, safe(issue.Message))
		}
		if len(result.Devices) == 0 {
			fmt.Fprintln(stderr, "No device confirmed. Run doctor or specify a known IP with --target / --via.")
		}
	}
	for _, d := range result.Devices {
		if d.Connected {
			return 0
		}
	}
	return 1
}

// Names and errors are device-controlled text; never print terminal escapes.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || (r >= 127 && r <= 159) {
			return ' '
		}
		return r
	}, s)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "--version" {
		fmt.Fprintln(stdout, "linkr-now", buildVersion)
		return 0
	}
	command := args[0]
	if command != "scan" && command != "info" && command != "watch" && command != "doctor" && command != "version" {
		fmt.Fprintln(stderr, "Unknown command:", safe(command))
		return 2
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	opts := linkr.DefaultOptions()
	jsonMode := fs.Bool("json", false, "JSON output")
	fs.Var(durationValue{&opts.Timeout}, "timeout", "request timeout")
	duration := 60 * time.Second
	latestVersion, latestURL := "", linkr.LatestVersionURL
	var targets stringsValue
	if command == "scan" || command == "watch" || command == "version" {
		fs.StringVar(&opts.Via, "via", opts.Via, "LAN gateway")
		fs.StringVar(&opts.USBHost, "usb-host", opts.USBHost, "USB network endpoint")
		fs.BoolVar(&opts.NoUSB, "no-usb", false, "skip USB discovery")
		fs.BoolVar(&opts.NoNetwork, "no-network", false, "skip gateway/multicast discovery")
		fs.BoolVar(&opts.NoMDNS, "no-mdns", false, "skip local multicast browsing")
		fs.Var(&targets, "target", "additional device origin")
	}
	if command == "watch" {
		fs.Var(durationValue{&duration}, "duration", "monitoring time")
	}
	if command == "version" {
		fs.StringVar(&latestVersion, "latest-version", "", "offline comparison version")
		fs.StringVar(&latestURL, "latest-url", latestURL, "release metadata URL")
	}
	if err := parse(fs, args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	opts.Targets = targets
	hosts := fs.Args()
	if command == "scan" {
		opts.Targets = append(opts.Targets, hosts...)
		return printResult(stdout, stderr, linkr.Scan(ctx, opts), *jsonMode)
	}
	if command == "info" {
		if len(hosts) != 1 {
			fmt.Fprintln(stderr, "info requires exactly one HOST")
			return 2
		}
		result := linkr.Result{Devices: []linkr.Device{}, Errors: []linkr.Issue{}}
		d, err := linkr.Info(ctx, hosts[0], "network", opts.Timeout)
		if err != nil {
			result.Errors = append(result.Errors, linkr.Issue{Source: "network", Target: hosts[0], Message: err.Error()})
		} else {
			result.Devices = append(result.Devices, d)
		}
		return printResult(stdout, stderr, result, *jsonMode)
	}
	if command == "doctor" {
		return doctor(ctx, hosts, opts.Timeout, *jsonMode, stdout)
	}
	if command == "version" {
		return version(ctx, hosts, opts, latestVersion, latestURL, *jsonMode, stdout, stderr)
	}
	watchCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	devices := []linkr.Device{}
	if len(hosts) > 0 {
		for _, host := range append(hosts, opts.Targets...) {
			d, err := linkr.Target(host, "network")
			if err != nil {
				fmt.Fprintln(stderr, safe(err.Error()))
				return 2
			}
			devices = append(devices, d)
		}
	} else {
		result := linkr.Scan(watchCtx, opts)
		devices = result.Devices
		for _, issue := range result.Errors {
			if *jsonMode {
				encode(stdout, map[string]any{"event": "ERROR", "error": issue})
			} else {
				fmt.Fprintf(stderr, "%s: %s\n", safe(issue.Target), safe(issue.Message))
			}
		}
		if !opts.NoUSB {
			hasUSB := false
			for _, d := range devices {
				hasUSB = hasUSB || d.Source == "usb"
			}
			if !hasUSB {
				if d, err := linkr.Target(opts.USBHost, "usb"); err == nil {
					devices = append(devices, d)
				}
			}
		}
	}
	if len(devices) == 0 || watchCtx.Err() != nil {
		return 1
	}
	found := false
	linkr.Watch(watchCtx, devices, opts.Timeout, func(event linkr.WatchEvent) {
		if event.Event == "INFO" {
			found = true
		}
		if *jsonMode {
			encode(stdout, event)
		} else {
			fmt.Fprintf(stdout, "%s %-13s %s %s %s\n", event.Timestamp, event.Event,
				event.Device.Domain, event.Device.Status, safe(event.Message))
		}
	})
	if found {
		return 0
	}
	return 1
}

func doctor(ctx context.Context, hosts []string, timeout time.Duration, jsonMode bool, out io.Writer) int {
	if len(hosts) == 0 {
		hosts = []string{"linkr.local", "linkr-usb.local"}
	}
	checks := []map[string]any{}
	ok := false
	for _, host := range hosts {
		check := map[string]any{"target": host, "resolvedAddresses": []string{}, "mdnsAddresses": []string{}, "errors": []string{}}
		errors := []string{}
		base, err := linkr.BaseURL(host)
		if err == nil {
			u, _ := url.Parse(base)
			dnsCtx, cancel := context.WithTimeout(ctx, timeout)
			addresses, dnsErr := net.DefaultResolver.LookupHost(dnsCtx, u.Hostname())
			cancel()
			if dnsErr == nil {
				check["resolvedAddresses"] = addresses
			} else {
				errors = append(errors, dnsErr.Error())
			}
			// Compare both resolvers even when DNS succeeds: a VPN or proxy may
			// return a synthetic address for a local multicast hostname.
			if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), ".local") {
				mdnsCtx, mdnsCancel := context.WithTimeout(ctx, 2*time.Second)
				addresses, mdnsErr := linkr.ResolveMDNS(mdnsCtx, u.Hostname())
				mdnsCancel()
				if mdnsErr == nil {
					check["mdnsAddresses"] = addresses
				} else {
					errors = append(errors, mdnsErr.Error())
				}
			}
			device, infoErr := linkr.Info(ctx, base, "network", timeout)
			if infoErr == nil {
				check["device"] = device
				ok = true
			} else {
				errors = append(errors, infoErr.Error())
			}
		} else {
			errors = append(errors, err.Error())
		}
		check["errors"] = errors
		checks = append(checks, check)
	}
	if jsonMode {
		encode(out, map[string]any{"checks": checks})
	} else {
		for _, check := range checks {
			fmt.Fprintf(out, "%s: DNS=%v mDNS=%v\n", safe(check["target"].(string)), check["resolvedAddresses"], check["mdnsAddresses"])
			if d, exists := check["device"].(linkr.Device); exists {
				fmt.Fprintf(out, "  HTTP OK: %s %s %s\n", safe(d.Name), d.Status, safe(d.Version))
			}
			for _, message := range check["errors"].([]string) {
				fmt.Fprintln(out, "  "+safe(message))
			}
		}
	}
	if ok {
		return 0
	}
	return 1
}

func version(ctx context.Context, hosts []string, opts linkr.Options, suppliedVersion, latestURL string, jsonMode bool, out, stderr io.Writer) int {
	release := map[string]any{"version": suppliedVersion}
	if suppliedVersion == "" {
		requestCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		var err error
		release, err = linkr.RequestJSON(requestCtx, latestURL)
		cancel()
		if err != nil {
			if jsonMode {
				encode(out, map[string]any{"errors": []linkr.Issue{{Source: "release", Target: latestURL, Message: err.Error()}}})
			} else {
				fmt.Fprintln(stderr, safe(err.Error()))
			}
			return 1
		}
	}
	latest, _ := release["version"].(string)
	if _, valid := linkr.CompareVersions(latest, latest); !valid {
		if jsonMode {
			encode(out, map[string]any{"errors": []linkr.Issue{{Source: "release", Target: latestURL, Message: "missing or invalid release version"}}})
		} else {
			fmt.Fprintln(stderr, "Missing or invalid release version")
		}
		return 1
	}
	result := linkr.Result{Devices: []linkr.Device{}, Errors: []linkr.Issue{}}
	if len(hosts) == 0 {
		result = linkr.Scan(ctx, opts)
	} else {
		for _, host := range append(hosts, opts.Targets...) {
			d, err := linkr.Info(ctx, host, "network", opts.Timeout)
			if err != nil {
				result.Errors = append(result.Errors, linkr.Issue{Source: "network", Target: host, Message: err.Error()})
			} else {
				result.Devices = append(result.Devices, d)
			}
		}
	}
	comparisons := []map[string]any{}
	for _, d := range result.Devices {
		comparison, valid := linkr.CompareVersions(d.Version, latest)
		state := "unknown"
		if valid && d.Connected {
			state = "current"
			if comparison < 0 {
				state = "outdated"
			} else if comparison > 0 {
				state = "newer"
			}
		}
		comparisons = append(comparisons, map[string]any{"device": d, "comparison": state})
	}
	if jsonMode {
		encode(out, map[string]any{"release": release, "devices": comparisons, "errors": result.Errors})
	} else {
		fmt.Fprintln(out, "Release:", safe(latest))
		for _, item := range comparisons {
			d := item["device"].(linkr.Device)
			fmt.Fprintf(out, "%s %s %s\n", d.Domain, safe(d.Version), item["comparison"])
		}
		for _, issue := range result.Errors {
			fmt.Fprintf(stderr, "%s: %s\n", safe(issue.Target), safe(issue.Message))
		}
	}
	for _, d := range result.Devices {
		if d.Connected {
			return 0
		}
	}
	return 1
}
