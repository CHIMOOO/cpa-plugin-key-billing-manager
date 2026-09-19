package turnstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"cpa-key-billing/internal/messages"
)

// A page fits comfortably below typical management gateway response limits,
// even when a single 4096-byte URL needs JSON HTML escaping.
const MaxProxyPageBytes = 32 << 10

var ErrProxyConfigChanged = messages.Errorf("The proxy configuration changed; reload the proxy lists")

type ProxyPage struct {
	Pool       string   `json:"pool"`
	Revision   string   `json:"revision"`
	Total      int      `json:"total"`
	Offset     int      `json:"offset"`
	NextOffset int      `json:"next_offset"`
	Done       bool     `json:"done"`
	Proxies    []string `json:"proxies"`
}

// ReadProxyPage is exclusively for authenticated management routes. Unlike
// Status, this explicitly requested response includes saved proxy credentials.
// One revision must be supplied for all subsequent pages, including the other
// pool, so a caller cannot assemble a configuration from mixed snapshots.
func (m *Manager) ReadProxyPage(pool string, offset int, revision string) (ProxyPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.configRevisionTokenLocked()
	if revision != "" && revision != current {
		return ProxyPage{}, ErrProxyConfigChanged
	}
	if offset < 0 || (offset > 0 && revision == "") {
		return ProxyPage{}, messages.Errorf("Invalid proxy list page")
	}
	var proxies []string
	switch pool {
	case "static":
		proxies = m.state.Config.ProbeProxies
	case "rotating":
		proxies = m.state.Config.ProbeProxiesRotating
	default:
		return ProxyPage{}, messages.Errorf("Proxy pool must be static or rotating")
	}
	if offset > len(proxies) {
		return ProxyPage{}, messages.Errorf("Invalid proxy list page")
	}
	page := ProxyPage{Pool: pool, Revision: current, Total: len(proxies), Offset: offset,
		NextOffset: offset, Proxies: []string{}}
	// Reserve more than the maximum envelope length and array punctuation.
	bytesLeft := MaxProxyPageBytes - 512
	for page.NextOffset < len(proxies) && len(page.Proxies) < 256 {
		value := proxies[page.NextOffset]
		encoded, _ := json.Marshal(value)
		if len(encoded)+1 > bytesLeft {
			break
		}
		page.Proxies = append(page.Proxies, value)
		page.NextOffset++
		bytesLeft -= len(encoded) + 1
	}
	page.Done = page.NextOffset == len(proxies)
	return page, nil
}

// Hashing the canonical persisted configuration also prevents revision ABA
// across host restarts. Cache by the internal counter because a large pool can
// need many pages; hashing the entire 16 MiB on every page is unnecessary.
func (m *Manager) configRevisionTokenLocked() string {
	if m.revisionToken != "" && m.revisionTokenAt == m.configRevision {
		return m.revisionToken
	}
	raw, _ := json.Marshal(m.state.Config)
	hash := sha256.Sum256(raw)
	m.revisionToken = hex.EncodeToString(hash[:])
	m.revisionTokenAt = m.configRevision
	return m.revisionToken
}
