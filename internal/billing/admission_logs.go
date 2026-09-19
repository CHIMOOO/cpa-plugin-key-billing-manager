package billing

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ReportQuotaBlock records that a key was turned away for an exhausted
// subscription. It is the only trace such a request leaves: enforcement runs
// before the request reaches an upstream, so nothing is billed and nothing is
// logged for it otherwise.
func (s *Store) ReportQuotaBlock(scope, endpoint string, decision Decision) {
	if decision.Allowed {
		return
	}
	scope = strings.TrimSpace(scope)
	if scope == "" || !s.blocked.onset(scope, decision.blockSignature()) {
		return
	}
	name := ""
	s.read(func(state *State) { name = state.describeKey(scope) })

	var message strings.Builder
	message.WriteString("Quota blocked: ")
	message.WriteString(name)
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		message.WriteString(" → ")
		message.WriteString(endpoint)
	}
	for _, window := range decision.Windows {
		for _, balance := range window.Dimensions {
			if balance.Blocked {
				fmt.Fprintf(&message, ", %s used %s (resets at %s)", window.Name,
					balance.Description(), window.EndAt.UTC().Format(time.RFC3339))
			}
		}
	}
	if plan := planName(decision); plan != "" {
		message.WriteString(", plan ")
		message.WriteString(plan)
	}
	if !decision.RetryAt.IsZero() {
		message.WriteString(", expected recovery ")
		message.WriteString(decision.RetryAt.UTC().Format(time.RFC3339))
	}
	// Enforcement working as configured is not a fault of the plugin's, so this
	// stays out of the level an operator reads to find one.
	s.AddPluginLog(PluginLogInfo, "%s", message.String())
}

type blockedKeys struct {
	mu     sync.Mutex
	states map[string]string
}

func (d Decision) blockSignature() string {
	var value strings.Builder
	fmt.Fprintf(&value, "%q", d.PlanID)
	for _, window := range d.Windows {
		if window.Blocked {
			fmt.Fprintf(&value, "|%q:%s", window.ID, window.StartAt.UTC().Format(time.RFC3339Nano))
			for _, balance := range window.Dimensions {
				if balance.Blocked {
					fmt.Fprintf(&value, ":%s", balance.Metric)
				}
			}
		}
	}
	return value.String()
}

func (b *blockedKeys) onset(scope, signature string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		b.states = make(map[string]string)
	}
	if previous, exists := b.states[scope]; exists && previous == signature {
		return false
	}
	b.states[scope] = signature
	return true
}

func (b *blockedKeys) clear(scope string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, scope)
}

func (b *blockedKeys) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states = nil
}

// describeKey names a key the way the panel does: the operator's remark beside
// the masked preview, either one alone when that is all there is, and the head
// of the scope for a key no synchronization has ever named.
func (s *State) describeKey(scope string) string {
	if description := keyDescription(s.Keys[scope]); description != "" {
		return description
	}
	if len(scope) > 12 {
		return scope[:12] + "…"
	}
	return scope
}

func keyDescription(key *KeyState) string {
	if key == nil {
		return ""
	}
	label, preview := strings.TrimSpace(key.Label), strings.TrimSpace(key.Preview)
	switch {
	case label != "" && preview != "":
		return label + " · " + preview
	case label != "":
		return label
	default:
		return preview
	}
}

func planName(decision Decision) string {
	if name := strings.TrimSpace(decision.PlanName); name != "" {
		return name
	}
	return strings.TrimSpace(decision.PlanID)
}
