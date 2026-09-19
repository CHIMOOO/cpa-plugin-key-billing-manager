package turnstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestProxyPagesRoundTripLargePoolsWithinByteLimit(t *testing.T) {
	m, _ := newTestManager(t)
	proxies := make([]string, 12000)
	for i := range proxies {
		proxies[i] = fmt.Sprintf("http://dummy:dummy-password@proxy-%d.example:8080", i)
	}
	// JSON HTML escaping can multiply this otherwise valid URL's byte size.
	rotating := []string{"http://" + strings.Repeat("&", 4000) + ":dummy@proxy.example:80"}
	raw, _ := json.Marshal(map[string]any{"probe_proxies": proxies, "probe_proxies_rotating": rotating})
	if err := m.Update(raw); err != nil {
		t.Fatal(err)
	}
	revision := ""
	for pool, want := range map[string][]string{"static": proxies, "rotating": rotating} {
		var got []string
		for offset := 0; ; {
			page, err := m.ReadProxyPage(pool, offset, revision)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(page)
			if len(encoded) > MaxProxyPageBytes || page.Pool != pool || page.Offset != offset || page.Total != len(want) || (revision != "" && page.Revision != revision) {
				t.Fatalf("invalid bounded page: bytes=%d, page=%+v", len(encoded), page)
			}
			revision = page.Revision
			got = append(got, page.Proxies...)
			if page.Done {
				break
			}
			if page.NextOffset <= offset {
				t.Fatal("page did not advance")
			}
			offset = page.NextOffset
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s pool was changed while paging", pool)
		}
	}
	status, _ := json.Marshal(m.Status())
	if strings.Contains(string(status), "dummy-password") || strings.Contains(string(status), "proxy-0.example") {
		t.Fatal("ordinary status exposed saved proxy URLs")
	}
}

func TestProxyPagesRejectMixedRevisionsAndInvalidOffsets(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://dummy:password@proxy.example:80"]}`)); err != nil {
		t.Fatal(err)
	}
	first, err := m.ReadProxyPage("static", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	first.Proxies[0] = "changed"
	unchanged, _ := m.ReadProxyPage("static", 0, first.Revision)
	if unchanged.Proxies[0] == "changed" {
		t.Fatal("returned page aliases the saved configuration")
	}
	for _, input := range []struct {
		pool     string
		offset   int
		revision string
	}{
		{"unknown", 0, ""}, {"static", -1, ""}, {"static", 1, ""}, {"static", 2, first.Revision},
	} {
		if _, err := m.ReadProxyPage(input.pool, input.offset, input.revision); err == nil {
			t.Fatalf("accepted invalid page %+v", input)
		}
	}
	if err := m.Update([]byte(`{"probe_proxies_rotating":["socks5h://proxy.example:1080"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadProxyPage("rotating", 0, first.Revision); !errors.Is(err, ErrProxyConfigChanged) {
		t.Fatalf("mixed pool revisions were accepted: %v", err)
	}
	current, _ := m.ReadProxyPage("static", 0, "")
	restarted := New()
	if err := restarted.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	loaded, _ := restarted.ReadProxyPage("static", 0, current.Revision)
	if loaded.Revision != current.Revision || loaded.Revision == first.Revision {
		t.Fatal("canonical revision changed across restart or reused an obsolete configuration")
	}
}
