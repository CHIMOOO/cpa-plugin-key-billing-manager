package turnstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLargeFailedBucketDoesNotStarveOtherAccountsOrModels(t *testing.T) {
	m, now := newTestManager(t)
	proxies := make([]string, 500)
	for i := range proxies {
		proxies[i] = fmt.Sprintf("http://proxy-%d.invalid:8080", i)
	}
	raw, _ := json.Marshal(map[string]any{"probe_accounts": []string{"account-a", "account-b"}, "models": []string{"model-a", "model-b"}, "probe_proxies": proxies})
	if err := m.Update(raw); err != nil {
		t.Fatal(err)
	}
	m.runProbe = degradedProbe
	want := []string{"account-a/model-a", "account-a/model-b", "account-b/model-a", "account-b/model-b"}
	for i := 0; i < 8; i++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" || result.Account+"/"+result.Model != want[i%len(want)] || result.ProxyIndex != i/4+1 {
			t.Fatalf("bucket/exit fairness attempt %d: %+v %v", i, result, err)
		}
		*now = now.Add(3 * time.Second)
	}
}

func TestSuccessfulRotatingBucketGetsNewBudgetAtRenewal(t *testing.T) {
	for _, failures := range []int{0, 9} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"ttl_seconds":60,"probe_proxies_rotating":["http://rotating.invalid:8080"]}`)); err != nil {
				t.Fatal(err)
			}
			calls := 0
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				calls++
				if calls <= failures {
					return degradedProbe(Credential{}, "", "")
				}
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			for i := 0; i < failures; i++ {
				if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" {
					t.Fatal(result, err)
				}
				*now = now.Add(3 * time.Second)
			}
			for i := 0; i < 15; i++ {
				result, err := m.Probe("", "", dummyCredential)
				if err != nil || result.Action != "harvested" || (i > 0 && result.ProxyAttempt != 1) {
					t.Fatalf("short-TTL renewal %d got blocked by successful attempts: %+v %v", i, result, err)
				}
				*now = now.Add(45 * time.Second)
			}
		})
	}
}

func TestAccountRefusalStaysPausedWhenFinalPersistenceFails(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"],"probe_proxies_rotating":["http://rotating.invalid:8080"]}`)); err != nil {
				t.Fatal(err)
			}
			writes, calls := 0, 0
			m.writeState = func(path string, raw []byte) error {
				writes++
				if writes == 2 {
					return errors.New("simulated final persistence failure")
				}
				return atomicWriteState(path, raw)
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				calls++
				return ProbeResponse{Status: status}, nil
			}
			if _, err := m.Probe("account-a", "model-a", dummyCredential); err == nil {
				t.Fatal("final persistence failure was hidden")
			}
			*now = now.Add(3 * time.Second)
			if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "cooling" || calls != 1 {
				t.Fatalf("disk failure re-hit a refused account: %+v %v calls=%d", result, err, calls)
			}
			if err := m.PersistLearned(); err != nil {
				t.Fatal(err)
			}
			restarted := readRestarted(t, m)
			restarted.runProbe = m.runProbe
			if result, err := restarted.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "cooling" {
				t.Fatal("retried persistence did not preserve known refusal", result, err)
			}
			if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Account != "account-b" || calls != 2 {
				t.Fatal("refused account blocked unrelated bucket", result, err)
			}
		})
	}
}

func TestRotatingBudgetIsSharedAcrossURLsAndSurvivesRestartAndConfigChanges(t *testing.T) {
	m, now := newTestManager(t)
	proxies := make([]string, 300)
	for i := range proxies {
		proxies[i] = fmt.Sprintf("socks5://dummy-session-%d:dummy@proxy.invalid:3000", i)
	}
	raw, _ := json.Marshal(map[string]any{"probe_proxies_rotating": proxies})
	if err := m.Update(raw); err != nil {
		t.Fatal(err)
	}
	m.runProbe = degradedProbe
	for i := 1; i <= 10; i++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" || result.ProxyAttempt != i || result.ProxyIndex != i {
			t.Fatalf("pooled attempt %d=%+v %v", i, result, err)
		}
		*now = now.Add(3 * time.Second)
	}
	for _, restarted := range []bool{false, true} {
		if restarted {
			m = readRestarted(t, m)
			m.runProbe = degradedProbe
		}
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
			t.Fatalf("more than ten attempts in one window (restart=%v): %+v %v", restarted, result, err)
		}
	}
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://brand-new.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("new proxy bypassed existing bucket budget", result, err)
	}
	*now = now.Add(10 * time.Minute)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" || result.ProxyAttempt != 1 {
		t.Fatal("new budget window did not reset", result, err)
	}
}

func TestRenewalsHavePriorityWithoutStarvingMissingBuckets(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["missing-a","missing-b","renew-a","renew-b"],"probe_proxies_rotating":["http://rotating.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"renew-a", "renew-b"} {
		learnFor(t, m, account, account, "model-a", now.Add(-56*time.Minute))
	}
	m.runProbe = degradedProbe
	var seen []string
	for i := 0; i < 8; i++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" {
			t.Fatal(result, err)
		}
		seen = append(seen, result.Account)
		*now = now.Add(3 * time.Second)
	}
	if strings.Join(seen, ",") != "renew-a,renew-b,renew-a,missing-a,renew-b,renew-a,renew-b,missing-b" {
		t.Fatalf("renewal priority/fairness=%v", seen)
	}
}

func TestLegacyPerURLBudgetsCannotMultiplyAfterUpgrade(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:80","http://second.invalid:80"]}`)); err != nil {
		t.Fatal(err)
	}
	for _, proxy := range m.state.Config.ProbeProxiesRotating {
		m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, true)] = cooldown{Until: now.Add(5 * time.Minute), Attempts: 5}
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		t.Fatal("legacy total attempts exceeded ten")
		return ProbeResponse{}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal(result, err)
	}
}

func TestFairnessSkipsFreshAndRejectedAccountWithoutLosingCursor(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b","account-c"],"probe_proxies_rotating":["http://rotating.invalid:80"]}`)); err != nil {
		t.Fatal(err)
	}
	selected := ""
	fetch := func(account string) (Credential, error) { selected = account; return dummyCredential(account) }
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		if selected == "account-b" {
			return ProbeResponse{Status: 429}, nil
		}
		return degradedProbe(Credential{}, "", "")
	}
	for _, wanted := range []string{"account-a", "account-b", "account-c", "account-a", "account-c"} {
		result, err := m.Probe("", "", fetch)
		if err != nil || result.Account != wanted {
			t.Fatalf("want %s: %+v %v", wanted, result, err)
		}
		*now = now.Add(3 * time.Second)
	}
}
