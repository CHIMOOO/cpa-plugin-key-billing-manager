package turnstate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func assertBusinessTemplate(t *testing.T, m *Manager, account, model, value string) {
	t.Helper()
	if !m.BusinessReady(account, model) {
		t.Fatal("a still-valid bucket was removed from business routing")
	}
	headers, _, refusal := m.BeforeRequired("continuity-business", account, model, nil)
	if refusal != "" || headers.Get(Header) != value {
		t.Fatalf("business template changed or became unavailable: refusal=%q got_len=%d want_len=%d", refusal, len(headers.Get(Header)), len(value))
	}
	m.Complete("continuity-business")
}

func continuityManager(t *testing.T) (*Manager, *time.Time, string) {
	t.Helper()
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"inject_mode":"always","renew_before_minutes":10}`)); err != nil {
		t.Fatal(err)
	}
	old := tokenAt(now.Add(-50 * time.Minute))
	learn(t, m, old)
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	return m, now, old
}

func TestEarlyRenewalKeepsServingOldTemplateUntilNewSnapshotCommits(t *testing.T) {
	m, now, old := continuityManager(t)
	newToken := tokenAt(*now)
	networkEntered, networkRelease := make(chan struct{}), make(chan struct{})
	writeEntered, writeRelease := make(chan struct{}), make(chan struct{})
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		close(networkEntered)
		<-networkRelease
		return ProbeResponse{Status: 200, Value: newToken}, nil
	}
	writes := 0
	m.writeState = func(path string, raw []byte) error {
		writes++
		if writes == 2 {
			close(writeEntered)
			<-writeRelease
		}
		return atomicWriteState(path, raw)
	}
	type outcome struct {
		result ProbeResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := m.Probe("", "", dummyCredential); done <- outcome{result, err} }()
	<-networkEntered
	assertBusinessTemplate(t, m, "account-a", "model-a", old)
	close(networkRelease)
	<-writeEntered
	// A valid replacement has arrived, but its disk write is deliberately
	// suspended. Business requests and a restart still use the committed old
	// template; there is no empty interval or prematurely published new token.
	assertBusinessTemplate(t, m, "account-a", "model-a", old)
	assertBusinessTemplate(t, readRestarted(t, m), "account-a", "model-a", old)
	close(writeRelease)
	result := <-done
	if result.err != nil || result.result.Action != "harvested" {
		t.Fatal(result.result, result.err)
	}
	assertBusinessTemplate(t, m, "account-a", "model-a", newToken)
	assertBusinessTemplate(t, readRestarted(t, m), "account-a", "model-a", newToken)
}

func TestUnsuccessfulEarlyRenewalPreservesOriginalTemplateButNeverExtendsExpiry(t *testing.T) {
	for _, failure := range []string{"transport", "degraded", "upstream-500", "unauthorized", "quota", "same", "older", "save"} {
		t.Run(failure, func(t *testing.T) {
			m, now, old := continuityManager(t)
			originalExpiry := now.Add(10 * time.Minute)
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				assertBusinessTemplate(t, m, "account-a", "model-a", old)
				switch failure {
				case "transport":
					return ProbeResponse{}, errors.New("simulated transport failure")
				case "degraded":
					return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
				case "upstream-500":
					return ProbeResponse{Status: 500}, nil
				case "unauthorized":
					return ProbeResponse{Status: 401}, nil
				case "quota":
					return ProbeResponse{Status: 429}, nil
				case "same":
					return ProbeResponse{Status: 200, Value: old}, nil
				case "older":
					return ProbeResponse{Status: 200, Value: tokenAt(now.Add(-51 * time.Minute))}, nil
				default:
					return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
				}
			}
			writes := 0
			if failure == "save" {
				m.writeState = func(path string, raw []byte) error {
					writes++
					if writes == 2 {
						return errors.New("simulated renewal save failure")
					}
					return atomicWriteState(path, raw)
				}
			}
			result, err := m.Probe("", "", dummyCredential)
			if (err != nil) != (failure == "save") || result.Action == "harvested" {
				t.Fatal("invalid renewal was published as success", result, err)
			}
			assertBusinessTemplate(t, m, "account-a", "model-a", old)
			assertBusinessTemplate(t, readRestarted(t, m), "account-a", "model-a", old)
			if rows := m.Status().Templates; len(rows) != 1 || !rows[0].ExpiresAt.Equal(originalExpiry) {
				t.Fatal("failed/same/older renewal changed the original expiration")
			}
			*now = originalExpiry.Add(-time.Second)
			assertBusinessTemplate(t, m, "account-a", "model-a", old)
			*now = originalExpiry
			headers, _, refusal := m.BeforeRequired("expired", "account-a", "model-a", nil)
			if m.BusinessReady("account-a", "model-a") || refusal == "" || len(headers) != 0 {
				t.Fatal("expired State was reused to fake continuity")
			}
		})
	}
}

func TestTenAndTwentyMinuteRenewalTriggersBeforeExpiryForEveryPool(t *testing.T) {
	for _, lead := range []int{10, 20} {
		for _, pool := range []string{"direct", "static", "rotating"} {
			t.Run(fmt.Sprintf("%dm/%s", lead, pool), func(t *testing.T) {
				m, now := newTestManager(t)
				if err := m.Update([]byte(fmt.Sprintf(`{"inject_mode":"always","renew_before_minutes":%d}`, lead))); err != nil {
					t.Fatal(err)
				}
				if pool != "direct" {
					field := "probe_proxies"
					if pool == "rotating" {
						field += "_rotating"
					}
					if err := m.Update([]byte(fmt.Sprintf(`{"%s":["http://proxy.invalid:8080"]}`, field))); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
					calls++
					return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
				}
				old := tokenAt(*now)
				renewAt := now.Add(time.Hour - time.Duration(lead)*time.Minute)
				if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
					t.Fatal(result, err)
				}
				*now = renewAt.Add(-time.Second)
				if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "fresh" || !result.NextCheckAt.Equal(renewAt) || calls != 1 {
					t.Fatal("renewal began too early or advertised expiration instead of renewal time", result, err)
				}
				assertBusinessTemplate(t, m, "account-a", "model-a", old)
				*now = renewAt
				if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" || calls != 2 {
					t.Fatal("configured lead did not trigger a renewal before expiration", result, err)
				}
				assertBusinessTemplate(t, m, "account-a", "model-a", tokenAt(*now))
			})
		}
	}
}

func TestCollectorLeaseTakeoverGapFitsLeadWithoutDiscardingOldState(t *testing.T) {
	// The runner's 90-second lease is outside the State core. Model that
	// heartbeat interruption here; this is not a claim that all queue/network
	// delays are bounded or that a 15-second automatic lead can cover a crash.
	for _, lead := range []int{10, 20} {
		t.Run(fmt.Sprint(lead), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(fmt.Sprintf(`{"inject_mode":"always","renew_before_minutes":%d}`, lead))); err != nil {
				t.Fatal(err)
			}
			old := tokenAt(*now)
			learn(t, m, old)
			if err := m.PersistLearned(); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Hour - time.Duration(lead)*time.Minute + 95*time.Second)
			assertBusinessTemplate(t, m, "account-a", "model-a", old)
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
				t.Fatal("brief collector handoff removed the existing valid template", result, err)
			}
			assertBusinessTemplate(t, m, "account-a", "model-a", tokenAt(*now))
		})
	}
}
