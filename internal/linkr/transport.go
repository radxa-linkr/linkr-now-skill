// Package linkr implements the HTTP JSON and SSE protocol used by linkr.now.
package linkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxResponseBytes = 1024 * 1024

// LatestVersionURL is the same release manifest used by linkr.now.
const LatestVersionURL = "https://raw.githubusercontent.com/radxa-linkr/linkr/refs/heads/main/v1/linkr/ota/latest"

var directClient = newDirectClient()

func newDirectClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Device traffic must never use HTTP_PROXY or the operating system's proxy.
	transport.Proxy = nil
	transport.DialContext = dialDirect
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func dialDirect(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return dialDirectWith(ctx, network, address, ResolveMDNS, dialer.DialContext)
}

func dialDirectWith(ctx context.Context, network, address string,
	resolve func(context.Context, string) ([]string, error),
	dial func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, port, splitError := net.SplitHostPort(address)
	if splitError != nil || !strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".local") {
		return dial(ctx, network, address)
	}
	// Query the local interfaces before system DNS, which can be redirected
	// through a VPN or return a proxy's synthetic address for .local names.
	resolveContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	addresses, resolveError := resolve(resolveContext, host)
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var mdnsError error
	switch {
	case resolveError != nil:
		mdnsError = fmt.Errorf("mDNS resolution failed: %w", resolveError)
	case len(addresses) == 0:
		mdnsError = fmt.Errorf("no mDNS response within 2s for %s", host)
	default:
		for _, ip := range addresses {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			connection, err := dial(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return connection, nil
			}
			mdnsError = fmt.Errorf("mDNS connection to %s failed: %w", ip, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, err := dial(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("%v; system connection failed: %w", mdnsError, err)
	}
	return connection, nil
}

var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// BaseURL normalizes a hostname, IP literal or HTTP(S) origin.
// Credentials, non-root paths, queries and fragments are rejected.
func BaseURL(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("a device hostname, IP address or HTTP(S) origin is required")
	}
	for _, character := range host {
		if character <= 32 || character == 127 {
			return "", errors.New("host must not contain whitespace or control characters")
		}
	}
	if !strings.Contains(host, "://") {
		if net.ParseIP(host) != nil && strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("invalid device host: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("device origin must use http:// or https://")
	}
	if u.User != nil {
		return "", errors.New("credentials in device origins are not supported")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(host, "#") {
		return "", errors.New("supply only the origin, without a path, query or fragment")
	}
	hostname := strings.ToLower(u.Hostname())
	address := hostname
	if zone := strings.LastIndex(address, "%"); zone >= 0 {
		address = address[:zone]
	}
	if net.ParseIP(address) == nil && !hostnamePattern.MatchString(hostname) {
		return "", errors.New("invalid device hostname or IP address")
	}
	if strings.Contains(hostname, ":") && net.ParseIP(address) == nil {
		return "", errors.New("invalid IPv6 address")
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("device port must be between 1 and 65535")
		}
		u.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		u.Host = "[" + hostname + "]"
	} else {
		u.Host = hostname
	}
	u.Path, u.RawPath = "", ""
	return u.String(), nil
}

func get(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	return getWithClient(ctx, directClient, endpoint, accept)
}

func getWithClient(ctx context.Context, client *http.Client, endpoint, accept string) (*http.Response, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("request requires an HTTP(S) URL without credentials")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "linkr-now-skill/1")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %w", endpoint, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", response.StatusCode, endpoint)
	}
	return response, nil
}

func jsonObject(raw []byte) (map[string]any, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) {
		return nil, errors.New("response is not valid UTF-8")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	if result == nil {
		return nil, errors.New("expected a JSON object")
	}
	return result, nil
}

// RequestJSON performs a direct GET and reads one bounded JSON object.
func RequestJSON(ctx context.Context, endpoint string) (map[string]any, error) {
	response, err := get(ctx, endpoint, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", endpoint, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("JSON response exceeds %d bytes", maxResponseBytes)
	}
	return jsonObject(raw)
}

// APIData validates the device's {code, message, data} response envelope.
func APIData(ctx context.Context, base, path string) (map[string]any, error) {
	base, err := BaseURL(base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, errors.New("API path must start with a single slash")
	}
	envelope, err := RequestJSON(ctx, base+path)
	if err != nil {
		return nil, err
	}
	code, validCode := envelope["code"].(float64)
	if !validCode || code != 0 {
		return nil, fmt.Errorf("device API error %v: %v", envelope["code"], envelope["message"])
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil, errors.New("device API response is missing an object-valued data field")
	}
	return data, nil
}

// StreamEvents reads /linkr SSE messages until an error or context cancellation.
// The outer JSON message is delivered intact; its data value may itself be JSON.
// A peer close is an error so the caller can reconnect with backoff.
func StreamEvents(ctx context.Context, base string, emit func(map[string]any) error) error {
	return StreamEventsWithTimeout(ctx, base, 10*time.Second, emit)
}

// StreamEventsWithTimeout limits response-header waiting independently of the
// stream's lifetime. Forty-five seconds without a recognized Linkr event cancels
// an idle stream; the caller decides whether and when to reconnect.
func StreamEventsWithTimeout(ctx context.Context, base string, headerTimeout time.Duration, emit func(map[string]any) error) error {
	return streamEvents(ctx, base, headerTimeout, 45*time.Second, emit)
}

func streamEvents(ctx context.Context, base string, headerTimeout, idleTimeout time.Duration, emit func(map[string]any) error) error {
	if headerTimeout <= 0 || idleTimeout <= 0 {
		return errors.New("SSE header and idle timeouts must be positive")
	}
	base, err := BaseURL(base)
	if err != nil {
		return err
	}
	client := newDirectClient()
	transport := client.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = headerTimeout
	defer transport.CloseIdleConnections()
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	response, err := getWithClient(streamCtx, client, base+"/linkr", "text/event-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	mediaType := strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(mediaType, "text/event-stream") {
		return fmt.Errorf("expected text/event-stream, got %q", mediaType)
	}
	idleError := fmt.Errorf("SSE heartbeat timeout: no Linkr event for %s", idleTimeout)
	idleTimer := time.AfterFunc(idleTimeout, func() { cancel(idleError) })
	defer idleTimer.Stop()
	stopOnCancellation := context.AfterFunc(streamCtx, func() { response.Body.Close() })
	defer stopOnCancellation()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxResponseBytes+1)
	dataLines := []string{}
	eventName := "message"
	frameBytes := 0
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		frameBytes += len(line) + 1
		if frameBytes > maxResponseBytes {
			return fmt.Errorf("SSE frame exceeds %d bytes", maxResponseBytes)
		}
		if !utf8.ValidString(line) {
			return errors.New("SSE frame is not valid UTF-8")
		}
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			if len(dataLines) > 0 && eventName == "message" {
				message, err := jsonObject([]byte(strings.Join(dataLines, "\n")))
				if err != nil {
					return fmt.Errorf("invalid SSE data: %w", err)
				}
				switch message["event"] {
				case "INFO", "CLICK", "RESET":
					idleTimer.Reset(idleTimeout)
				}
				if err := emit(message); err != nil {
					return err
				}
			}
			dataLines = dataLines[:0]
			eventName, frameBytes = "message", 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			eventName = value
			if eventName == "" {
				eventName = "message"
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cause := context.Cause(streamCtx); cause != nil {
		return cause
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("SSE read failed: %w", err)
	}
	return errors.New("SSE stream closed by device")
}

type versionIdentifier struct {
	text   string
	number string
}

type parsedVersion struct {
	core       [3]string
	prerelease []versionIdentifier
}

var versionPattern = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var alphaNumberPattern = regexp.MustCompile(`^([a-zA-Z]+)(\d+)$`)
var numberPattern = regexp.MustCompile(`^\d+$`)

func normalizeNumber(value string) string {
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0"
	}
	return value
}

func parseVersion(value string) (parsedVersion, bool) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return parsedVersion{}, false
	}
	version := parsedVersion{core: [3]string{normalizeNumber(match[1]), normalizeNumber(match[2]), normalizeNumber(match[3])}}
	if match[4] == "" {
		return version, true
	}
	for _, identifier := range strings.Split(match[4], ".") {
		if parts := alphaNumberPattern.FindStringSubmatch(identifier); parts != nil {
			version.prerelease = append(version.prerelease, versionIdentifier{text: strings.ToLower(parts[1])}, versionIdentifier{number: normalizeNumber(parts[2])})
		} else if numberPattern.MatchString(identifier) {
			version.prerelease = append(version.prerelease, versionIdentifier{number: normalizeNumber(identifier)})
		} else {
			version.prerelease = append(version.prerelease, versionIdentifier{text: strings.ToLower(identifier)})
		}
	}
	return version, true
}

func compareNumbers(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

// CompareVersions follows linkr.now's stable/prerelease ordering, including beta2.
// Invalid versions return valid=false rather than claiming equality.
func CompareVersions(current, latest string) (comparison int, valid bool) {
	left, leftOK := parseVersion(current)
	right, rightOK := parseVersion(latest)
	if !leftOK || !rightOK {
		return 0, false
	}
	for index := range left.core {
		if result := compareNumbers(left.core[index], right.core[index]); result != 0 {
			return result, true
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0, true
	}
	if len(left.prerelease) == 0 {
		return 1, true
	}
	if len(right.prerelease) == 0 {
		return -1, true
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		first, second := left.prerelease[index], right.prerelease[index]
		var result int
		switch {
		case first.number != "" && second.number != "":
			result = compareNumbers(first.number, second.number)
		case first.number != "":
			result = -1
		case second.number != "":
			result = 1
		default:
			result = strings.Compare(first.text, second.text)
		}
		if result != 0 {
			return result, true
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1, true
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1, true
	}
	return 0, true
}
