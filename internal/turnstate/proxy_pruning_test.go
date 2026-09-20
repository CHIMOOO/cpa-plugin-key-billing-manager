package turnstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const pruningProxyA = "socks5://dummy-session-a:dummy-password@gateway.invalid:3000"
const pruningProxyB = "socks5://dummy-session-b:dummy-password@gateway.invalid:3000"

func configurePruning(t *testing.T, m *Manager, patch map[string]any) {
	t.Helper()
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Update(raw); err != nil {
		t.Fatal(err)
	}
}

func degradedProbe(Credential, string, string) (ProbeResponse, error) {
	return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
}

func TestProxyPruningDefaultsAndLegacyState(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := m.Status().Config
	if cfg.ProbeDropFailedProxies || cfg.ProbeDropDegradedProxies || cfg.ProbeMinProxies != 10 {
		t.Fatalf("destructive policy enabled by default: %+v", cfg)
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err = json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	legacy := disk["config"].(map[string]any)
	for _, key := range []string{"probe_drop_failed_proxies", "probe_drop_degraded_proxies", "probe_min_proxies"} {
		delete(legacy, key)
	}
	raw, _ = json.Marshal(disk)
	if err = os.WriteFile(m.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New()
	if err = restarted.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	cfg = restarted.Status().Config
	if cfg.ProbeDropFailedProxies || cfg.ProbeDropDegradedProxies || cfg.ProbeMinProxies != 10 {
		t.Fatalf("legacy settings did not retain safe defaults: %+v", cfg)
	}
	for _, minimum := range []int{-1, 0, 40001} {
		if err = m.Update([]byte(`{"probe_min_proxies":` + strconv.Itoa(minimum) + `}`)); err == nil {
			t.Fatalf("unsafe minimum %d accepted", minimum)
		}
	}
}

func TestProxyPruningIsOptInAndNeverRemovesAccountFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		response   ProbeResponse
		fetchError bool
	}{
		{name: "disabled-degraded", response: ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}},
		{name: "disabled-network", response: ProbeResponse{ProxyFailure: true}},
		{name: "missing-credential", enabled: true, fetchError: true},
		{name: "account-unauthorized", enabled: true, response: ProbeResponse{Status: 401}},
		{name: "account-forbidden", enabled: true, response: ProbeResponse{Status: 403}},
		{name: "account-quota", enabled: true, response: ProbeResponse{Status: 429, Value: strings.Repeat("x", 312)}},
		{name: "account-status-overrides-failure-hint", enabled: true, response: ProbeResponse{Status: 401, ProxyFailure: true}},
		{name: "upstream-unavailable", enabled: true, response: ProbeResponse{Status: 503}},
		{name: "unknown-state", enabled: true, response: ProbeResponse{Status: 200, Value: "unknown"}},
		{name: "invalid-timestamp", enabled: true, response: ProbeResponse{Status: 200, Value: strings.Repeat("x", 292)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1,
				"probe_drop_failed_proxies": tc.enabled, "probe_drop_degraded_proxies": tc.enabled})
			page, _ := m.ReadProxyPage("static", 0, "")
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return tc.response, nil }
			fetch := dummyCredential
			if tc.fetchError {
				fetch = func(string) (Credential, error) { return Credential{}, errors.New("dummy expired account") }
			}
			result, err := m.Probe("", "", fetch)
			if err != nil || result.ProxyDisposition != "" || result.ProxyConfigRevision != "" {
				t.Fatalf("unexpected pruning result: %+v, %v", result, err)
			}
			after, err := m.ReadProxyPage("static", 0, page.Revision)
			if err != nil || !reflect.DeepEqual(page.Proxies, after.Proxies) {
				t.Fatalf("unqualified error changed proxy configuration: %+v, %v", after, err)
			}
		})
	}
}

func TestProxyPruningPoliciesAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failed, degraded bool
		response         ProbeResponse
		want             string
	}{
		{"failed-only-retains-degraded", true, false, ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, ""},
		{"degraded-only-retains-network", false, true, ProbeResponse{ProxyFailure: true}, ""},
		{"failed-removes-proxy-auth", true, false, ProbeResponse{Status: 407, ProxyFailure: true}, "removed"},
		{"degraded-removes-confirmed-state", false, true, ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1,
				"probe_drop_failed_proxies": tc.failed, "probe_drop_degraded_proxies": tc.degraded})
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return tc.response, nil }
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.ProxyDisposition != tc.want {
				t.Fatalf("result = %+v, %v", result, err)
			}
		})
	}
	t.Run("pool-below-default-floor", func(t *testing.T) {
		m, _ := newTestManager(t)
		configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_drop_degraded_proxies": true})
		m.runProbe = degradedProbe
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.ProxyDisposition != "retained_minimum" || result.ProxyRemaining != 2 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
}

func TestProxyPruningUsesExactURLPoolAndCombinedFloor(t *testing.T) {
	for _, rotating := range []bool{false, true} {
		t.Run(strconv.FormatBool(rotating), func(t *testing.T) {
			m, now := newTestManager(t)
			configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB},
				"probe_proxies_rotating": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 3, "probe_drop_degraded_proxies": true})
			if rotating {
				for _, proxy := range []string{pruningProxyA, pruningProxyB} {
					m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, false)] = cooldown{Until: now.Add(time.Hour)}
				}
			}
			m.runProbe = degradedProbe
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.Action != "degraded" || result.ProxyDisposition != "removed" || result.ProxyRemaining != 3 || result.ProxyConfigRevision == "" {
				t.Fatalf("selected entry not removed: %+v, %v", result, err)
			}
			selected, other := "static", "rotating"
			if rotating {
				selected, other = other, selected
			}
			page, err := m.ReadProxyPage(selected, 0, result.ProxyConfigRevision)
			if err != nil || !reflect.DeepEqual(page.Proxies, []string{pruningProxyB}) {
				t.Fatalf("wrong session removed: %+v, %v", page, err)
			}
			page, err = m.ReadProxyPage(other, 0, result.ProxyConfigRevision)
			if err != nil || !reflect.DeepEqual(page.Proxies, []string{pruningProxyA, pruningProxyB}) {
				t.Fatalf("other pool changed: %+v, %v", page, err)
			}
			*now = now.Add(3 * time.Second)
			result, err = m.Probe("", "", dummyCredential)
			if err != nil || result.ProxyDisposition != "retained_minimum" || result.ProxyRemaining != 3 || result.ProxyConfigRevision != "" {
				t.Fatalf("combined minimum not respected: %+v, %v", result, err)
			}
			restarted := New()
			if err = restarted.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
				t.Fatal(err)
			}
			page, err = restarted.ReadProxyPage(selected, 0, "")
			if err != nil || !reflect.DeepEqual(page.Proxies, []string{pruningProxyB}) {
				t.Fatalf("removal did not survive restart: %+v, %v", page, err)
			}
		})
	}
}

func TestProxyPruningCannotFallBackToDirect(t *testing.T) {
	m, now := newTestManager(t)
	configurePruning(t, m, map[string]any{"probe_proxies_rotating": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1, "probe_drop_failed_proxies": true})
	m.runProbe = func(_ Credential, _, proxy string) (ProbeResponse, error) {
		if proxy == "" {
			t.Fatal("automatic removal enabled a direct probe")
		}
		return ProbeResponse{ProxyFailure: true}, context.DeadlineExceeded
	}
	for i := 0; i < 12; i++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.ProxyRemaining != 1 || (result.ProxyDisposition != "removed" && result.ProxyDisposition != "retained_minimum") {
			t.Fatalf("floor probe %d = %+v, %v", i, result, err)
		}
		*now = now.Add(11 * time.Minute)
	}
}

func TestProxyPruningRollsBackFailedPersistence(t *testing.T) {
	m, _ := newTestManager(t)
	configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1, "probe_drop_degraded_proxies": true})
	page, _ := m.ReadProxyPage("static", 0, "")
	path := m.path
	failPath := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(failPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var committed []byte
	m.runProbe = func(credential Credential, model, proxy string) (ProbeResponse, error) {
		committed, _ = os.ReadFile(path)
		m.path = failPath
		return degradedProbe(credential, model, proxy)
	}
	result, err := m.Probe("", "", dummyCredential)
	m.path = path
	if err != nil || result.Action != "error" || result.ProxyDisposition != "retained_persistence_error" || result.ProxyRemaining != 2 || result.ProxyConfigRevision != "" {
		t.Fatalf("failed removal was not reported safely: %+v, %v", result, err)
	}
	after, err := m.ReadProxyPage("static", 0, page.Revision)
	if err != nil || !reflect.DeepEqual(after.Proxies, page.Proxies) {
		t.Fatalf("failed removal changed memory/revision: %+v, %v", after, err)
	}
	disk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(committed, disk) {
		t.Fatal("failed removal changed committed state", err)
	}
	m.runProbe = degradedProbe
	result, err = m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "degraded" || result.ProxyDisposition != "removed" {
		t.Fatalf("failed removal stopped subsequent collection: %+v, %v", result, err)
	}
}

func TestProxyPruningRejectsStaleEditsAndUploads(t *testing.T) {
	m, _ := newTestManager(t)
	configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1, "probe_drop_degraded_proxies": true})
	page, _ := m.ReadProxyPage("static", 0, "")
	edit, _ := json.Marshal(map[string]any{"expected_revision": page.Revision, "probe_proxies": page.Proxies})
	upload, err := m.BeginConfigUpload(len(edit))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.AppendConfigUpload(upload.ID, 0, edit); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	m.runProbe = func(credential Credential, model, proxy string) (ProbeResponse, error) {
		close(started)
		<-finish
		return degradedProbe(credential, model, proxy)
	}
	probed := make(chan error, 1)
	go func() { _, err := m.Probe("", "", dummyCredential); probed <- err }()
	<-started
	updated := make(chan error, 1)
	go func() { updated <- m.Update(edit) }()
	select {
	case err := <-updated:
		close(finish)
		<-probed
		t.Fatalf("configuration overtook the active probe: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	if err = <-probed; err != nil {
		t.Fatal(err)
	}
	if err = <-updated; err == nil {
		t.Fatal("stale save restored an automatically removed proxy")
	}
	if err = m.CommitConfigUpload(upload.ID); err == nil {
		t.Fatal("stale chunk upload restored an automatically removed proxy")
	}
	if _, err = m.ReadProxyPage("static", 0, page.Revision); !errors.Is(err, ErrProxyConfigChanged) {
		t.Fatalf("stale pages accepted after removal: %v", err)
	}
	current, _ := m.ReadProxyPage("static", 0, "")
	if !reflect.DeepEqual(current.Proxies, []string{pruningProxyB}) {
		t.Fatal("stale edit overwrote selected pool")
	}
}

func TestProxyPruningStatusCarriesCurrentRevision(t *testing.T) {
	m, now := newTestManager(t)
	configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_min_proxies": 1, "probe_drop_degraded_proxies": true})
	before := m.Status()
	page, err := m.ReadProxyPage("static", 0, before.ProxyConfigRevision)
	if err != nil || before.ProxyConfigRevision == "" || before.ProxyConfigRevision != page.Revision {
		t.Fatalf("status cannot identify the loaded proxy revision: %+v, %v", page, err)
	}
	m.runProbe = degradedProbe
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.ProxyDisposition != "removed" {
		t.Fatalf("probe = %+v, %v", result, err)
	}
	after := m.Status()
	if after.ProxyConfigRevision == before.ProxyConfigRevision || after.ProxyConfigRevision != result.ProxyConfigRevision {
		t.Fatal("status retained the revision from before automatic removal")
	}
	page, err = m.ReadProxyPage("static", 0, after.ProxyConfigRevision)
	if err != nil || page.Revision != after.ProxyConfigRevision || page.Total != 1 {
		t.Fatalf("new status/page mismatch: %+v, %v", page, err)
	}
	// A later result can overwrite last_probe after a response was lost. The
	// independent status revision must still expose that committed deletion.
	*now = now.Add(3 * time.Second)
	result, err = m.Probe("", "", dummyCredential)
	if err != nil || result.ProxyDisposition != "retained_minimum" {
		t.Fatalf("next probe = %+v, %v", result, err)
	}
	current := m.Status()
	if current.LastProbe.ProxyConfigRevision != "" || current.ProxyConfigRevision != after.ProxyConfigRevision {
		t.Fatal("a non-removing result hid the committed configuration revision")
	}
	configurePruning(t, m, map[string]any{"probe_proxies": []string{pruningProxyA}})
	updated := m.Status()
	if updated.ProxyConfigRevision == after.ProxyConfigRevision {
		t.Fatal("same-count proxy change did not update status revision")
	}
	page, err = m.ReadProxyPage("static", 0, updated.ProxyConfigRevision)
	if err != nil || !reflect.DeepEqual(page.Proxies, []string{pruningProxyA}) {
		t.Fatalf("status missed externally replaced pool: %+v, %v", page, err)
	}
}

func TestProbeProxyFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   bool
	}{
		{"connect-auth", errors.New("proxy error"), 407, true},
		{"connect-policy", &net.OpError{Op: "proxyconnect", Err: errors.New("denied")}, 403, false},
		{"connect-rate-limit", context.DeadlineExceeded, 429, false},
		{"socks-auth", errors.New("socks connect: username/password authentication failed"), 0, true},
		{"dial", &net.OpError{Op: "dial", Err: errors.New("refused")}, 0, true},
		{"timeout", context.DeadlineExceeded, 200, true},
		{"eof", io.ErrUnexpectedEOF, 200, true},
		{"local-credential", errors.New("net/http: invalid header field value for Authorization"), 0, false},
		{"tls-trust", errors.New("tls: failed to verify certificate"), 200, false},
		{"no-error", nil, 407, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeProxyFailure(tc.err, tc.status); got != tc.want {
				t.Fatalf("classification = %t; want %t", got, tc.want)
			}
		})
	}
	for _, status := range []int{401, 403, 407, 429, 502} {
		t.Run("real-connect-"+strconv.Itoa(status), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Error("expected CONNECT")
				}
				w.WriteHeader(status)
			}))
			defer proxy.Close()
			credential, _ := dummyCredential("")
			response, err := runHTTPProbe(credential, "model-a", proxy.URL)
			if err == nil || response.ProxyFailure != (status == 407) {
				t.Fatalf("real CONNECT %d = %+v, %v", status, response, err)
			}
		})
	}
}

func TestProbePruningClassifiesRealSOCKSAuthenticationFailure(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				done <- rejectSOCKSAuthentication(connection)
			}()
			credential, _ := dummyCredential("")
			response, err := runHTTPProbe(credential, "model-a", scheme+"://dummy:dummy-password@"+listener.Addr().String())
			if err == nil || !response.ProxyFailure {
				t.Errorf("SOCKS failure = %+v, %v", response, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
