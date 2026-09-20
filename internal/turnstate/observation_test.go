package turnstate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResponseObservationSeparatesInjectionAndLength(t *testing.T) {
	for _, injected := range []bool{false, true} {
		for _, length := range []int{0, 292, 312, 333} {
			t.Run(fmt.Sprintf("injected=%t/length=%d", injected, length), func(t *testing.T) {
				m, now := newTestManager(t)
				if err := m.Update([]byte(`{"inject_mode":"always","learn_responses":false}`)); err != nil {
					t.Fatal(err)
				}
				if injected {
					m.state.Templates[key("account-a", "model-a")] = Template{Account: "account-a", Model: "model-a", Value: tokenAt(*now), IssuedAt: *now}
				}
				headers, _ := m.Before("request", "account-a", "model-a", nil)
				if (headers.Get(Header) != "") != injected {
					t.Fatal("test did not exercise requested injection behavior")
				}
				if err := m.Learn("request", "", "client-alias", http.Header{Header: {strings.Repeat("x", length)}}); err != nil {
					t.Fatal(err)
				}
				status := m.Status().Observations
				if length == 0 && !injected {
					if len(status.Events) != 0 || len(status.Buckets) != 0 {
						t.Fatal("unmodified silent response is not a state observation")
					}
					return
				}
				kind := map[int]string{0: "silent", 292: "match", 312: "replace", 333: "other"}[length]
				if len(status.Events) != 1 || len(status.Buckets) != 1 {
					t.Fatalf("missing observation: %+v", status)
				}
				event := status.Events[0]
				if event.Account != "account-a" || event.Model != "model-a" || event.Length != length || event.Kind != kind || event.Injected != injected {
					t.Fatalf("wrong exact attribution: %+v", event)
				}
				counts := observationIncrement(injected, kind)
				if status.Buckets[0].Counts != counts || status.Buckets[0].Recent24h != counts {
					t.Fatal("lifetime and hourly classification disagree")
				}
				if len(m.dirtyTemplates) != 0 {
					t.Fatal("observation enabled response learning")
				}
				// A duplicate response hook may not double-count a request.
				_ = m.Learn("request", "account-a", "model-a", http.Header{Header: {strings.Repeat("x", length)}})
				if len(m.Status().Observations.Events) != 1 {
					t.Fatal("duplicate response counted twice")
				}
			})
		}
	}
}

func TestObservationUsesActualWriteAndPreservesSignedLength(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	m.observations = observationState{}
	for _, mode := range []string{"dry-run", "replace-only", "client-template", "injected"} {
		if err := m.Update([]byte(`{"inject_mode":"always","dry_run":false,"learn_responses":false}`)); err != nil {
			t.Fatal(err)
		}
		var incoming http.Header
		switch mode {
		case "dry-run":
			if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
				t.Fatal(err)
			}
		case "replace-only":
			if err := m.Update([]byte(`{"inject_mode":"replace-only"}`)); err != nil {
				t.Fatal(err)
			}
		case "client-template":
			incoming = http.Header{Header: {tokenAt(*now)}}
		}
		m.Before(mode, "account-a", "model-a", incoming)
		_ = m.Learn(mode, "", "", http.Header{Header: {strings.Repeat("x", 333)}})
		event := m.Status().Observations.Events[0]
		if event.Injected != (mode == "injected") || event.Kind != "other" {
			t.Fatalf("%s fabricated injection or a good state: %+v", mode, event)
		}
	}
	m.Before("silent", "account-a", "model-a", nil)
	_ = m.Learn("silent", "", "", nil)
	view := m.Status().Observations.Buckets[0]
	if view.Last.Length != 0 || view.LastSigned == nil || view.LastSigned.Length != 333 || view.LastSigned.Kind != "other" {
		t.Fatalf("silence replaced last signed observation: %+v", view)
	}
	view.LastSigned.Length = 123
	if m.Status().Observations.Buckets[0].LastSigned.Length != 333 {
		t.Fatal("status exposes mutable observation state")
	}
}

func TestObservationRejectsStaleOrUnverifiableAttribution(t *testing.T) {
	for _, scenario := range []string{"missing", "mismatch", "retry-empty", "completed", "expired", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			m, now := newTestManager(t)
			m.Before("request", "account-a", "model-a", nil)
			account := ""
			switch scenario {
			case "missing":
				delete(m.pending, "request")
			case "mismatch":
				account = "other-account"
			case "retry-empty":
				m.Before("request", "", "model-a", nil)
			case "completed":
				m.Complete("request")
			case "expired":
				*now = now.Add(16 * time.Minute)
			case "disabled":
				if err := m.Update([]byte(`{"enabled":false}`)); err != nil {
					t.Fatal(err)
				}
			}
			_ = m.Learn("request", account, "model-a", http.Header{Header: {strings.Repeat("x", 312)}})
			if len(m.Status().Observations.Events) != 0 || len(m.pending) != 0 {
				t.Fatal("invalid attribution was retained or observed")
			}
		})
	}
}

func TestObservationRetryReplacesAccountAndActualInjectionFlag(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	m.observations = observationState{}
	if err := m.Update([]byte(`{"inject_mode":"always","learn_responses":false}`)); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("retry", "account-a", "model-a", nil); headers.Get(Header) == "" {
		t.Fatal("first attempt did not inject")
	}
	if headers, _ := m.Before("retry", "account-b", "model-b", nil); headers.Get(Header) != "" {
		t.Fatal("retry used another account's template")
	}
	_ = m.Learn("retry", "", "client-alias", http.Header{Header: {strings.Repeat("x", 312)}})
	status := m.Status().Observations
	if len(status.Events) != 1 || status.Events[0].Account != "account-b" || status.Events[0].Model != "model-b" || status.Events[0].Injected {
		t.Fatalf("retry inherited previous attempt's attribution or injection: %+v", status)
	}
}

func TestObservationUsesConfiguredLengthsWithoutLoggingValues(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"template_length":300,"replace_length":320,"learn_responses":false}`)); err != nil {
		t.Fatal(err)
	}
	for length, kind := range map[int]string{292: "other", 300: "match", 312: "other", 320: "replace"} {
		m.Before("request", "account-a", "model-a", nil)
		value := strings.Repeat("s", length)
		_ = m.Learn("request", "", "", http.Header{Header: {value}})
		status := m.Status().Observations
		if status.Events[0].Kind != kind {
			t.Fatal("classification ignored configured lengths")
		}
		raw, err := json.Marshal(status)
		if err != nil || strings.Contains(string(raw), value) {
			t.Fatal("diagnostic contains a state value or cannot serialize")
		}
	}
}

func TestObservationBoundsAndHourlyWindow(t *testing.T) {
	m, now := newTestManager(t)
	for i := 0; i < 300; i++ {
		account := fmt.Sprintf("account-%03d", i)
		m.Before("request", account, "model-a", nil)
		_ = m.Learn("request", "", "", http.Header{Header: {strings.Repeat("x", 312)}})
		*now = now.Add(time.Second)
	}
	status := m.Status().Observations
	if len(status.Buckets) != observationBucketLimit || len(status.Events) != observationEventLimit || status.Events[0].Account != "account-299" || status.Events[99].Account != "account-200" {
		t.Fatal("observation bounds or event order changed")
	}
	if _, ok := m.observations.Buckets[key("account-000", "model-a")]; ok {
		t.Fatal("oldest observation bucket not evicted")
	}
	m.observations = observationState{}
	for i := 0; i < 60; i++ {
		m.Before("request", "account-a", "model-a", nil)
		_ = m.Learn("request", "", "", http.Header{Header: {strings.Repeat("x", 312)}})
		if i < 59 {
			*now = now.Add(time.Hour)
		}
	}
	status = m.Status().Observations
	if len(m.observations.Buckets[key("account-a", "model-a")].Hours) != observationHourLimit || status.Buckets[0].Counts.NativeReplace != 60 || status.Buckets[0].Recent24h.NativeReplace != 24 {
		t.Fatalf("hourly retention or rollup incorrect: %+v", status.Buckets[0])
	}
	*now = now.Add(24 * time.Hour)
	if m.Status().Observations.Buckets[0].Recent24h.NativeReplace != 0 {
		t.Fatal("24-hour summary retained expired hours without new traffic")
	}
}

func TestObservationVolatileLifecycleAndNoBusinessIO(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"learn_responses":false}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	m.writeState = func(string, []byte) error { t.Fatal("observation performed disk I/O"); return nil }
	m.Before("request", "account-a", "model-a", nil)
	_ = m.Learn("request", "", "", http.Header{Header: {strings.Repeat("x", 312)}})
	after, err := os.ReadFile(m.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("observation persisted business-path data")
	}
	m.writeState = nil
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Observations.Events) != 1 {
		t.Fatal("ordinary changes cleared diagnostics")
	}
	restarted := New()
	if err := restarted.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if len(restarted.Status().Observations.Events) != 0 {
		t.Fatal("volatile diagnostics survived restart")
	}
	if err := m.Configure(filepath.Join(t.TempDir(), "other.db")); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Observations.Events) != 0 {
		t.Fatal("new data path inherited old observations")
	}
}

func TestObservationConcurrentHooksAndStatus(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"learn_responses":false}`)); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 30; i++ {
				request := fmt.Sprintf("request-%d-%d", worker, i)
				account := fmt.Sprintf("account-%d", worker)
				m.Before(request, account, "model-a", nil)
				_ = m.Learn(request, "", "client-alias", http.Header{Header: {strings.Repeat("x", 312)}})
				_ = m.Status()
			}
		}(worker)
	}
	workers.Wait()
	for _, bucket := range m.Status().Observations.Buckets {
		if bucket.Counts.NativeReplace != 30 {
			t.Fatalf("concurrent request attribution lost: %+v", bucket)
		}
	}
}
