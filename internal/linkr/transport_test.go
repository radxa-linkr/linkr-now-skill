package linkr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDialDirectUsesMDNSBeforeSystemDNSAndPreservesHTTPHost(t *testing.T) {
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	hosts := make(chan string, 1)
	go func() {
		request, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			hosts <- err.Error()
			return
		}
		hosts <- request.Host
		fmt.Fprint(peer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()
	var calls []string
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialDirectWith(ctx, network, address,
			func(resolveCtx context.Context, host string) ([]string, error) {
				calls = append(calls, "resolve:"+host)
				deadline, ok := resolveCtx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					return nil, errors.New("mDNS lookup has no bounded deadline")
				}
				return []string{"192.0.2.10", "192.0.2.11"}, nil
			},
			func(_ context.Context, _ string, target string) (net.Conn, error) {
				calls = append(calls, "dial:"+target)
				if target == "192.0.2.11:8080" {
					return connection, nil
				}
				return nil, errors.New("connection refused")
			},
		)
	}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://Linkr.local.:8080/linkr/info")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if host := <-hosts; host != "Linkr.local.:8080" {
		t.Fatalf("HTTP Host = %q; want the requested hostname", host)
	}
	want := []string{"resolve:Linkr.local.", "dial:192.0.2.10:8080", "dial:192.0.2.11:8080"}
	if !slices.Equal(calls, want) {
		t.Fatalf("lookup order = %v; want %v", calls, want)
	}
}

func TestDialDirectFallsBackToSystem(t *testing.T) {
	systemError := errors.New("system connection failed")
	for _, test := range []struct {
		name         string
		addresses    []string
		resolveError error
		systemError  error
	}{
		{name: "mDNS unavailable", resolveError: errors.New("no multicast interface")},
		{name: "no mDNS response"},
		{name: "mDNS address unreachable", addresses: []string{"192.0.2.10"}},
		{name: "both fail", resolveError: errors.New("no multicast interface"), systemError: systemError},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, peer := net.Pipe()
			defer connection.Close()
			defer peer.Close()
			var calls []string
			got, err := dialDirectWith(context.Background(), "tcp", "linkr.local:80",
				func(context.Context, string) ([]string, error) {
					calls = append(calls, "resolve")
					return test.addresses, test.resolveError
				},
				func(_ context.Context, _ string, target string) (net.Conn, error) {
					calls = append(calls, target)
					if target == "linkr.local:80" {
						if test.systemError != nil {
							return nil, test.systemError
						}
						return connection, nil
					}
					return nil, errors.New("mDNS address unreachable")
				},
			)
			wantCalls := []string{"resolve"}
			for _, address := range test.addresses {
				wantCalls = append(wantCalls, address+":80")
			}
			wantCalls = append(wantCalls, "linkr.local:80")
			if !slices.Equal(calls, wantCalls) {
				t.Fatalf("lookup order = %v; want %v", calls, wantCalls)
			}
			if test.systemError != nil {
				if got != nil || !errors.Is(err, test.systemError) {
					t.Fatalf("fallback error = %v; want %v", err, test.systemError)
				}
			} else if err != nil || got != connection {
				t.Fatalf("fallback returned %v, %v; want the system connection", got, err)
			}
		})
	}
}

func TestDialDirectSkipsMDNSForOtherHosts(t *testing.T) {
	for _, address := range []string{"example.com:80", "192.0.2.10:80", "[::1]:80", "linkr.local.example:80"} {
		t.Run(address, func(t *testing.T) {
			connection, peer := net.Pipe()
			defer connection.Close()
			defer peer.Close()
			got, err := dialDirectWith(context.Background(), "tcp", address,
				func(context.Context, string) ([]string, error) {
					t.Error("non-local address triggered multicast resolution")
					return nil, nil
				},
				func(_ context.Context, network, target string) (net.Conn, error) {
					if network != "tcp" || target != address {
						t.Errorf("dial = %q, %q; want tcp, %q", network, target, address)
					}
					return connection, nil
				},
			)
			if err != nil || got != connection {
				t.Fatalf("dial returned %v, %v", got, err)
			}
		})
	}
}

func TestDialDirectCancellationStopsFurtherWork(t *testing.T) {
	for _, phase := range []string{"before lookup", "during lookup", "during first connection"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before lookup" {
				cancel()
			}
			resolutions, connections := 0, 0
			got, err := dialDirectWith(ctx, "tcp", "linkr.local:80",
				func(context.Context, string) ([]string, error) {
					resolutions++
					if phase == "during lookup" {
						cancel()
					}
					return []string{"192.0.2.10", "192.0.2.11"}, nil
				},
				func(context.Context, string, string) (net.Conn, error) {
					connections++
					cancel()
					return nil, context.Canceled
				},
			)
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled dial = %v, %v", got, err)
			}
			wantResolutions, wantConnections := 1, 0
			if phase == "before lookup" {
				wantResolutions = 0
			} else if phase == "during first connection" {
				wantConnections = 1
			}
			if resolutions != wantResolutions || connections != wantConnections {
				t.Fatalf("continued after cancellation: %d resolutions, %d connections", resolutions, connections)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	for _, test := range []struct{ input, expected string }{
		{"linkr.local", "http://linkr.local"},
		{"linkr.local.", "http://linkr.local."},
		{"http://LINKR.local:80/", "http://linkr.local:80"},
		{"192.0.2.1", "http://192.0.2.1"},
		{"::1", "http://[::1]"},
		{"https://[::1]:443", "https://[::1]:443"},
	} {
		actual, err := BaseURL(test.input)
		if err != nil || actual != test.expected {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", test.input, actual, err, test.expected)
		}
	}
	for _, input := range []string{"", "ftp://host", "http://user:pass@host", "http://host/path", "http://host?q=1", "http://host#fragment", "http://host?", "http://host:65536", "host\nname"} {
		if actual, err := BaseURL(input); err == nil {
			t.Errorf("BaseURL(%q) accepted as %q", input, actual)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, test := range []struct {
		left, right string
		comparison  int
		valid       bool
	}{
		{"v1.6.0", "1.6.0+build", 0, true},
		{"1.6.0-beta2", "1.6.0-beta10", -1, true},
		{"1.6.0-beta.2", "1.6.0-beta2", 0, true},
		{"1.6.0-beta2", "1.6.0", -1, true},
		{"1.6.0", "1.6.0-beta2", 1, true},
		{"1.6.0-1", "1.6.0-alpha", -1, true},
		{"2.0.0", "1.99.99", 1, true},
		{"garbage", "1.6.0", 0, false},
		{"1.6", "1.6.0", 0, false},
	} {
		actual, valid := CompareVersions(test.left, test.right)
		if actual != test.comparison || valid != test.valid {
			t.Errorf("CompareVersions(%q,%q) = %d,%v; want %d,%v", test.left, test.right, actual, valid, test.comparison, test.valid)
		}
	}
}

func TestAPIEnvelopeAndHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/linkr/info":
			fmt.Fprint(w, `{"code":0,"data":{"status":"IDLE","onlineTime":"123.5"}}`)
		case "/error":
			fmt.Fprint(w, `{"code":1,"message":"mdns failed","data":{}}`)
		case "/missing":
			fmt.Fprint(w, `{"data":{}}`)
		case "/boolean":
			fmt.Fprint(w, `{"code":false,"data":{}}`)
		case "/array":
			fmt.Fprint(w, `[]`)
		case "/oversize":
			fmt.Fprint(w, `{"padding":"`+strings.Repeat("a", maxResponseBytes)+`"}`)
		case "/invalid-utf8":
			w.Write([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
		case "/redirect":
			http.Redirect(w, r, "/linkr/info", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	data, err := APIData(context.Background(), server.URL, "/linkr/info")
	if err != nil || data["status"] != "IDLE" {
		t.Fatalf("APIData = %v, %v", data, err)
	}
	for _, path := range []string{"/error", "/missing", "/boolean"} {
		if _, err := APIData(context.Background(), server.URL, path); err == nil {
			t.Errorf("APIData accepted invalid envelope at %s", path)
		}
	}
	for _, path := range []string{"/array", "/oversize", "/invalid-utf8", "/redirect", "/404"} {
		if _, err := RequestJSON(context.Background(), server.URL+path); err == nil {
			t.Errorf("RequestJSON accepted %s", path)
		}
	}
}

func TestSSEMultilineAndNestedData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, "\ufeff: comment\r\nevent: message\r\ndata: {\"event\":\"INFO\",\r\ndata: \"data\":\"{\\\"status\\\":\\\"IDLE\\\"}\"}\r\n\r\n")
		fmt.Fprint(w, "event: ignored\ndata: not-json\n\n")
		fmt.Fprint(w, "data: {\"event\":\"RESET\",\"data\":\"\"}\n\n")
	}))
	defer server.Close()
	messages := []map[string]any{}
	err := StreamEvents(context.Background(), server.URL, func(message map[string]any) error {
		messages = append(messages, message)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected SSE close error, got %v", err)
	}
	if len(messages) != 2 || messages[0]["event"] != "INFO" || messages[0]["data"] != `{"status":"IDLE"}` || messages[1]["event"] != "RESET" {
		t.Fatalf("unexpected SSE messages: %v", messages)
	}
}

func TestSSECancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := StreamEvents(ctx, server.URL, func(map[string]any) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("SSE cancellation did not interrupt an idle read")
	}
}

func TestSSEHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The TCP connection is healthy but the peer never sends HTTP headers.
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := StreamEventsWithTimeout(ctx, server.URL, 80*time.Millisecond, func(map[string]any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("expected response-header timeout, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("response-header timeout did not interrupt the request")
	}
}

func TestSSEIdleWatchdog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		// Comments keep the socket alive without confirming a Linkr heartbeat.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, ": proxy keepalive\n\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := streamEvents(ctx, server.URL, time.Second, 100*time.Millisecond, func(map[string]any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "heartbeat timeout") {
		t.Fatalf("expected heartbeat timeout, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("idle watchdog did not interrupt the stream")
	}
}

func TestSSERecognizedEventsResetIdleWatchdog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, "data: {\"event\":\"RESET\",\"data\":\"\"}\n\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := errors.New("five events received")
	count := 0
	err := streamEvents(ctx, server.URL, 100*time.Millisecond, 200*time.Millisecond, func(map[string]any) error {
		count++
		if count == 5 {
			return done
		}
		return nil
	})
	if !errors.Is(err, done) {
		t.Fatalf("expected five events over a stream longer than header and idle timeouts; got %d events and %v", count, err)
	}
}
