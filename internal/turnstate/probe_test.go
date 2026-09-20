package turnstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dummyCredential(string) (Credential, error) {
	return Credential{AccessToken: "dummy-oauth-token", AccountID: "dummy-account-id"}, nil
}

func TestStaticAndRotatingProbeCooldownsPersist(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://static.invalid:8000"],"probe_proxies_rotating":["http://rotating.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	staticCalls, rotatingCalls := 0, 0
	m.runProbe = func(_ Credential, model, proxy string) (ProbeResponse, error) {
		if strings.Contains(proxy, "rotating") {
			rotatingCalls++
		} else {
			staticCalls++
		}
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	for i := 0; i < 11; i++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" {
			t.Fatalf("probe %d = %+v, %v", i, result, err)
		}
		*now = now.Add(3 * time.Second)
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "cooling" || staticCalls != 1 || rotatingCalls != 10 {
		t.Fatalf("retry budgets violated: static=%d rotating=%d result=%+v err=%v", staticCalls, rotatingCalls, result, err)
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	loaded.runProbe = m.runProbe
	if result, _ := loaded.Probe("", "", dummyCredential); result.Action != "cooling" {
		t.Fatal("restart lost cooldown")
	}
	*now = now.Add(11 * time.Minute)
	if _, err := loaded.Probe("", "", dummyCredential); err != nil {
		t.Fatal(err)
	}
	if staticCalls != 1 || rotatingCalls != 11 {
		t.Fatalf("incorrect per-pool cooldown: static=%d rotating=%d", staticCalls, rotatingCalls)
	}
}

func TestProbeHarvestRenewalAndAccountRefusals(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			m, now := newTestManager(t)
			calls := 0
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				calls++
				return ProbeResponse{Status: status, Value: tokenAt(*now)}, nil
			}
			result, err := m.Probe("", "", dummyCredential)
			if err != nil {
				t.Fatal(err)
			}
			if status == 200 {
				if result.Action != "harvested" || len(m.Status().Templates) != 1 {
					t.Fatalf("template not saved: %+v", result)
				}
				if result, _ = m.Probe("", "", dummyCredential); result.Action != "fresh" {
					t.Fatalf("fresh template reprobed: %+v", result)
				}
				*now = now.Add(55 * time.Minute)
				if result, _ = m.Probe("", "", dummyCredential); result.Action != "harvested" || calls != 2 {
					t.Fatalf("renewal did not resume: %+v", result)
				}
			} else {
				if result.Action != "error" || len(m.Status().Templates) != 0 || strings.Contains(result.Reason, "IP") {
					t.Fatalf("account refusal misclassified: %+v", result)
				}
				*now = now.Add(30 * time.Second)
				if result, _ = m.Probe("", "", dummyCredential); result.Action != "account_wait" || calls != 1 {
					t.Fatalf("account refusal retried too soon: %+v", result)
				}
			}
		})
	}
}

func TestProbeRenewalDoesNotReportOldTemplateAsNewHarvest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  func(time.Time) string
		action string
	}{
		{"malformed", func(time.Time) string { return strings.Repeat("x", 292) }, "error"},
		{"expired", func(now time.Time) string { return tokenAt(now.Add(-time.Hour)) }, "error"},
		{"future", func(now time.Time) string { return tokenAt(now.Add(time.Second)) }, "error"},
		{"same", func(now time.Time) string { return tokenAt(now.Add(-56 * time.Minute)) }, "unchanged"},
		{"older", func(now time.Time) string { return tokenAt(now.Add(-57 * time.Minute)) }, "unchanged"},
		{"newer", func(now time.Time) string { return tokenAt(now) }, "harvested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, now := newTestManager(t)
			oldIssued := now.Add(-56 * time.Minute)
			oldToken := tokenAt(oldIssued)
			learn(t, m, oldToken)
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: 200, Value: tc.value(*now)}, nil
			}
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.Action != tc.action {
				t.Fatalf("renewal result = %+v, %v; want action %s", result, err, tc.action)
			}
			status := m.Status()
			if len(status.Templates) != 1 {
				t.Fatalf("renewal lost existing template: %+v", status.Templates)
			}
			if tc.action == "harvested" {
				if !status.Templates[0].IssuedAt.Equal(*now) || status.Counters.Learned != 2 {
					t.Fatal("valid new template failed to renew")
				}
			} else if !status.Templates[0].IssuedAt.Equal(oldIssued) || !status.Templates[0].ExpiresAt.Equal(oldIssued.Add(time.Hour)) || status.Counters.Learned != 1 {
				t.Fatalf("unsuccessful renewal changed template lifetime/counters: %+v", status)
			}
		})
	}
}

func TestProbeDoesNotWriteAcrossReconfigure(t *testing.T) {
	m, now := newTestManager(t)
	started, finish := make(chan struct{}), make(chan struct{})
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		close(started)
		<-finish
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	probed := make(chan error, 1)
	go func() { _, err := m.Probe("", "", dummyCredential); probed <- err }()
	<-started
	if result, _ := m.Probe("", "", dummyCredential); result.Action != "busy" {
		t.Fatal("concurrent probe was not bounded")
	}
	configured := make(chan error, 1)
	newPath := filepath.Join(t.TempDir(), "other.db")
	go func() { configured <- m.Configure(newPath) }()
	close(finish)
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Templates) != 0 {
		t.Fatal("probe crossed state-file reconfiguration")
	}
}

func TestClearWaitsForInFlightProbe(t *testing.T) {
	m, now := newTestManager(t)
	started, finish := make(chan struct{}), make(chan struct{})
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		close(started)
		<-finish
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	probed := make(chan error, 1)
	go func() { _, err := m.Probe("", "", dummyCredential); probed <- err }()
	<-started
	cleared := make(chan error, 1)
	go func() { cleared <- m.Clear("", "") }()
	select {
	case err := <-cleared:
		close(finish)
		t.Fatalf("clear overtook in-flight probe: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Templates) != 0 {
		t.Fatal("in-flight probe restored cleared template")
	}
}

func TestProbePersistenceFailureRetainsKnownAccountRefusal(t *testing.T) {
	m, _ := newTestManager(t)
	directory := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		m.mu.Lock()
		m.path = directory
		m.mu.Unlock()
		return ProbeResponse{Status: 429}, nil
	}
	if _, err := m.Probe("", "", dummyCredential); err == nil {
		t.Fatal("expected persistence failure")
	}
	if _, exists := m.state.Cooldowns[accountKey("account-a")]; !exists {
		t.Fatal("failed persistence discarded a known account refusal")
	}
}

func TestProbeErrorsNeverExposeCredentials(t *testing.T) {
	m, _ := newTestManager(t)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{}, errors.New("dummy-password dummy-oauth-token")
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "error" || strings.Contains(result.Reason, "dummy") {
		t.Fatalf("unsafe error: %+v, %v", result, err)
	}
}

func TestProbeSkipsUnavailableAccountsBeforeSelectingBucket(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["deleted","account-a"]}`)); err != nil {
		t.Fatal(err)
	}
	called := ""
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	result, err := m.ProbeWithAvailability("", "", func(account string) bool { return account == "account-a" }, func(account string) (Credential, error) {
		called = account
		return dummyCredential(account)
	})
	if err != nil || result.Action != "harvested" || called != "account-a" {
		t.Fatalf("unavailable account consumed the next bucket: %+v called=%q err=%v", result, called, err)
	}
	result, err = m.ProbeWithAvailability("", "", func(string) bool { return false }, func(string) (Credential, error) {
		t.Fatal("read credentials for unavailable account")
		return Credential{}, nil
	})
	if err != nil || result.Action != "error" {
		t.Fatalf("all unavailable = %+v, %v", result, err)
	}
}

func TestCredentialClaimsExpiryAndAccountID(t *testing.T) {
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"exp": now.Add(time.Minute).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "claim-id"}})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	raw, _ := json.Marshal(map[string]string{"access_token": token, "account_id": "file-id"})
	credential, err := ParseCredential(raw, now)
	if err != nil || credential.AccountID != "claim-id" {
		t.Fatalf("claim account not preferred: %+v %v", credential, err)
	}
	if _, err := ParseCredential(raw, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired access token accepted")
	}
}
