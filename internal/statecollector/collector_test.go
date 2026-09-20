package statecollector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRecoversAuthenticationAndServerErrorsAndReloadsKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("dummy-old-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var inFlight atomic.Int32
	var observations []string
	var instanceID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inFlight.Add(1) != 1 {
			t.Error("collector issued concurrent requests")
		}
		defer inFlight.Add(-1)
		if r.Method != http.MethodPost || r.URL.Path != RunnerPath+"/tick" {
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 2 || payload["version"] != "v-test" || len(payload["instance_id"]) != 32 {
			t.Errorf("unexpected tick payload: %+v", payload)
		}
		if instanceID == "" {
			instanceID = payload["instance_id"]
		} else if payload["instance_id"] != instanceID {
			t.Error("identity changed between ticks")
		}
		observations = append(observations, r.Header.Get("Authorization"))
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(401)
			_, _ = w.Write([]byte("dummy-secret-response"))
		case 2:
			w.WriteHeader(500)
			_, _ = w.Write([]byte("dummy-secret-response"))
		case 3:
			_, _ = w.Write([]byte(`{"enabled":true,"phase":"waiting","next_tick_seconds":17,"last_error":"dummy-secret-response"}`))
		case 4:
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":{"code":"runner_owned","message":"dummy-secret-response"}}`))
		default:
			_, _ = w.Write([]byte(`{"enabled":false,"phase":"stopped","next_tick_seconds":5}`))
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	c, err := New(Config{URL: server.URL, ManagementKeyFile: keyPath, Version: "v-test", Logger: log.New(&output, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	c.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 1 {
			if err := os.WriteFile(keyPath, []byte("dummy-new-key\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if len(waits) == 5 {
			return context.Canceled
		}
		return nil
	}
	if err := c.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{3 * time.Second, 6 * time.Second, 17 * time.Second, 5 * time.Second, 5 * time.Second}) {
		t.Fatalf("retry/due waits: %v", waits)
	}
	if len(observations) != 5 || observations[0] != "Bearer dummy-old-key" {
		t.Fatalf("unexpected key count=%d", len(observations))
	}
	for _, key := range observations[1:] {
		if key != "Bearer dummy-new-key" {
			t.Fatal("key file was not reloaded")
		}
	}
	for _, secret := range []string{"dummy-old-key", "dummy-new-key", "dummy-secret-response", instanceID} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("collector logged private request/response data")
		}
	}
}

func TestCollectorCheckDoesNotStartOrClaimCollection(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "dummy-env-key")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodGet || r.URL.Path != RunnerPath || len(body) != 0 || r.Header.Get("Authorization") != "Bearer dummy-env-key" {
			t.Errorf("check changed the server: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"enabled":false,"phase":"stopped","online":false}`))
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	status, err := c.Check(context.Background())
	if err != nil || status.Enabled || status.Phase != "stopped" || requests != 1 {
		t.Fatalf("check result=%+v error=%v requests=%d", status, err, requests)
	}
	other, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if c.instanceID == other.instanceID {
		t.Fatal("process instances shared identity")
	}
}

func TestCollectorNeverFollowsRedirectsOrEnvironmentProxy(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "dummy-env-key")
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	t.Setenv("HTTP_PROXY", target.URL)
	t.Setenv("HTTPS_PROXY", target.URL)
	c, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("management traffic inherited environment proxies")
	}
	if _, err := c.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect accepted: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("management credential reached redirect/proxy destination")
	}
}

func TestCollectorCancellationInterruptsHTTPAndBackoff(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "dummy-env-key")
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the management request")
	}
	start := time.Now()
	if err := waitContext(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatal("canceled backoff did not stop immediately")
	}
}

func TestCollectorBoundsBackoffHintsAndRejectsInvalidOrigins(t *testing.T) {
	for _, version := range []string{"invalid version", strings.Repeat("v", 65), "-invalid", "invalid/version"} {
		if c, err := New(Config{Version: version}); err == nil {
			c.Close()
			t.Fatal("version incompatible with runner protocol was accepted")
		}
	}
	for _, input := range []string{"ftp://host", "http://dummy:secret@host", "http://host/path", "http://host/?key=dummy", "http://host/#secret", "http://host:99999", "http://host?", "http://host/%2F"} {
		if c, err := New(Config{URL: input}); err == nil {
			c.Close()
			t.Fatalf("invalid management origin accepted: %s", input)
		}
	}
	for _, test := range []struct{ value, want float64 }{{0, 5}, {-1, 5}, {.1, 1}, {1, 1}, {60, 60}, {900, 60}} {
		if got := tickDelay(test.value); got != time.Duration(test.want*float64(time.Second)) {
			t.Fatalf("hint %v became %v", test.value, got)
		}
	}
	c, err := New(Config{URL: "http://127.0.0.1:9", ManagementKeyFile: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	c.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 8 {
			return context.Canceled
		}
		return nil
	}
	_ = c.Run(context.Background())
	want := []time.Duration{3, 6, 12, 24, 48, 60, 60, 60}
	for i := range want {
		if waits[i] != want[i]*time.Second {
			t.Fatal("unbounded retry backoff", waits)
		}
	}
}

func TestCollectorRejectsUntrustedTLSAndMalformedStatusWithoutLoggingSecrets(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "dummy-sensitive-key")
	for _, response := range []string{`null`, `{}`, `{"phase":"dummy-response-secret"}`, `{"phase":"stopped"}`, `{"enabled":null,"phase":"stopped"}`, strings.Repeat("x", maxBody+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, response) }))
		c, err := New(Config{URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Check(context.Background())
		if err == nil || strings.Contains(err.Error(), "dummy-") {
			t.Fatalf("malformed response was accepted or exposed: %v", err)
		}
		c.Close()
		server.Close()
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"phase":"stopped"}`) }))
	defer server.Close()
	c, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Check(context.Background()); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
