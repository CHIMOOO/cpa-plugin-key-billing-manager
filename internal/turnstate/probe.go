package turnstate

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Credential is supplied for one probe, stays in memory and is never persisted.
type Credential struct {
	AccessToken string
	AccountID   string
}

type ProbeResponse struct {
	Status int
	Value  string
}

type ProbeResult struct {
	Action      string    `json:"action"`
	Reason      string    `json:"reason"`
	Account     string    `json:"account,omitempty"`
	Model       string    `json:"model,omitempty"`
	Exit        string    `json:"exit,omitempty"`
	Status      int       `json:"status,omitempty"`
	Length      int       `json:"length,omitempty"`
	NextCheckAt time.Time `json:"next_check_at"`
}

type probeCandidate struct {
	account, model, proxy, cooldownKey string
	rotating                           bool
}

func ProbeSupported() bool {
	_, err := exec.LookPath("curl")
	return err == nil
}

func proxyKey(account, model, proxy string, rotating bool) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t", account, model, proxy, rotating)))
	return "exit:" + fmt.Sprintf("%x", h)
}

func accountKey(account string) string {
	h := sha256.Sum256([]byte(account))
	return "account:" + fmt.Sprintf("%x", h)
}

// Probe performs at most one upstream request. The browser may call it again;
// there is no detached runner, goroutine or timer after this method returns.
// fetch must return only the specifically selected OAuth credential.
func (m *Manager) Probe(account, model string, fetch func(string) (Credential, error)) (ProbeResult, error) {
	return m.probe(account, model, nil, fetch)
}

// ProbeWithAvailability is Probe with a host-supplied snapshot of accounts
// that may currently be used. It prevents disabled/deleted accounts from
// consuming a cooldown bucket before fetch is called.
func (m *Manager) ProbeWithAvailability(account, model string, available func(string) bool, fetch func(string) (Credential, error)) (ProbeResult, error) {
	return m.probe(account, model, available, fetch)
}

func (m *Manager) probe(account, model string, available func(string) bool, fetch func(string) (Credential, error)) (ProbeResult, error) {
	model = ModelName(model)
	if !m.probeMu.TryLock() {
		return ProbeResult{Action: "busy", Reason: "已有探测请求正在进行", NextCheckAt: time.Now().Add(3 * time.Second)}, nil
	}
	defer m.probeMu.Unlock()
	m.mu.Lock()
	now := m.now()
	m.pruneLocked(now)
	candidate, result, ok := m.selectProbeLocked(account, model, now, available)
	if !ok {
		m.mu.Unlock()
		return result, nil
	}
	old, existed := m.state.Cooldowns[candidate.cooldownKey]
	reserved := cooldown{Until: now.Add(55 * time.Minute)}
	if candidate.rotating {
		reserved = cooldown{Until: now.Add(10 * time.Minute), Attempts: old.Attempts + 1}
	}
	m.state.Cooldowns[candidate.cooldownKey] = reserved
	if err := m.persistLocked(); err != nil {
		if existed {
			m.state.Cooldowns[candidate.cooldownKey] = old
		} else {
			delete(m.state.Cooldowns, candidate.cooldownKey)
		}
		m.mu.Unlock()
		return ProbeResult{}, err
	}
	m.mu.Unlock()
	credential, err := fetch(candidate.account)
	if err != nil {
		return m.finishProbe(candidate, ProbeResponse{}, "无法读取有效的 Codex OAuth 凭证；请重新登录或检查账号", 10*time.Minute)
	}
	response, err := m.runProbe(credential, candidate.model, candidate.proxy)
	if err != nil {
		// Never return curl stderr or OAuth/proxy credentials in transport errors.
		return m.finishProbe(candidate, response, "探测连接失败或超过 25 秒；请检查 curl、代理和网络", 2*time.Second)
	}
	return m.finishProbe(candidate, response, "", 2*time.Second)
}

func (m *Manager) selectProbeLocked(account, model string, now time.Time, available func(string) bool) (probeCandidate, ProbeResult, bool) {
	cfg := m.state.Config
	result := ProbeResult{Action: "cooling", Reason: "所有待采集桶的出口均在冷却", NextCheckAt: now.Add(time.Hour)}
	accounts, models := cfg.ProbeAccounts, cfg.Models
	if account != "" {
		if !contains(accounts, account) {
			return probeCandidate{}, ProbeResult{Action: "error", Reason: "账号不在已保存的探测范围内", NextCheckAt: now.Add(time.Minute)}, false
		}
		accounts = []string{account}
	}
	if model != "" {
		if !contains(models, model) {
			return probeCandidate{}, ProbeResult{Action: "error", Reason: "模型不在已保存的探测范围内", NextCheckAt: now.Add(time.Minute)}, false
		}
		models = []string{model}
	}
	if len(accounts) == 0 || len(models) == 0 {
		return probeCandidate{}, ProbeResult{Action: "error", Reason: "请先保存探测账号和模型", NextCheckAt: now.Add(time.Minute)}, false
	}
	statics := cfg.ProbeProxies
	if len(statics) == 0 && len(cfg.ProbeProxiesRotating) == 0 {
		statics = []string{""}
	}
	fresh := 0
	availableAccounts := 0
	for _, a := range accounts {
		if available != nil && !available(a) {
			continue
		}
		availableAccounts++
		for _, model := range models {
			if t, ok := m.state.Templates[key(a, model)]; ok {
				renewAt := t.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second - renewalLead(cfg.TTLSeconds))
				if renewAt.After(now) {
					fresh++
					if renewAt.Before(result.NextCheckAt) {
						result.NextCheckAt = renewAt
					}
					continue
				}
			}
			if c := m.state.Cooldowns[accountKey(a)]; c.Until.After(now) {
				if c.Until.Before(result.NextCheckAt) {
					result.NextCheckAt = c.Until
				}
				continue
			}
			for poolIndex, pool := range [][]string{statics, cfg.ProbeProxiesRotating} {
				for _, proxy := range pool {
					rotating := poolIndex == 1
					k := proxyKey(a, model, proxy, rotating)
					c := m.state.Cooldowns[k]
					if c.Until.After(now) && (!rotating || c.Attempts >= 10) {
						if c.Until.Before(result.NextCheckAt) {
							result.NextCheckAt = c.Until
						}
						continue
					}
					return probeCandidate{account: a, model: model, proxy: proxy, cooldownKey: k, rotating: rotating}, ProbeResult{}, true
				}
			}
		}
	}
	if available != nil && availableAccounts == 0 {
		return probeCandidate{}, ProbeResult{Action: "error", Reason: "没有可用的 Codex OAuth 账号", NextCheckAt: now.Add(time.Minute)}, false
	}
	if fresh == availableAccounts*len(models) {
		result.Action, result.Reason = "fresh", "全部模板仍有效，临近过期时再续采"
	}
	return probeCandidate{}, result, false
}

func renewalLead(ttl int) time.Duration {
	seconds := ttl / 4
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (m *Manager) finishProbe(c probeCandidate, response ProbeResponse, failure string, rest time.Duration) (ProbeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	result := ProbeResult{Account: c.account, Model: c.model, Exit: maskProxy(c.proxy), Status: response.Status,
		Length: len(response.Value), NextCheckAt: now.Add(2 * time.Second)}
	switch {
	case failure != "":
		result.Action, result.Reason = "error", failure
	case response.Status == 401 || response.Status == 403:
		result.Action, result.Reason = "error", "上游拒绝此账号；暂停该账号 55 分钟，请检查登录和权限"
		rest = 55 * time.Minute
	case response.Status == 429:
		result.Action, result.Reason = "error", "账号受到上游限流；暂停该账号 10 分钟"
		rest = 10 * time.Minute
	case response.Status != 200:
		result.Action, result.Reason = "error", fmt.Sprintf("上游返回 HTTP %d，未采集模板", response.Status)
	case len(response.Value) == m.state.Config.TemplateLength:
		issued, parsed := issuedAt(response.Value)
		incoming := Template{Account: c.account, Model: c.model, Value: response.Value, IssuedAt: issued}
		if !parsed || !m.usableLocked(incoming, now) {
			result.Action, result.Reason = "error", "响应长度符合，但 Fernet 时间戳无效、超前或已过期"
			break
		}
		if previous, exists := m.state.Templates[key(c.account, c.model)]; exists && m.usableLocked(previous, now) && !issued.After(previous.IssuedAt) {
			result.Action, result.Reason = "unchanged", "上游返回已有或更旧的模板；本次未续期，原到期时间保持不变"
			break
		}
		if err := m.learnLocked(c.account, c.model, response.Value, now); err != nil {
			return ProbeResult{}, err
		}
		if t, ok := m.state.Templates[key(c.account, c.model)]; ok && t.Value == response.Value && t.IssuedAt.Equal(issued) && m.usableLocked(t, now) {
			result.Action, result.Reason = "harvested", "已采集并保存此账号和模型的有效模板"
		} else {
			result.Action, result.Reason = "error", "本次响应模板未被保存，本次未续期"
		}
	case len(response.Value) == m.state.Config.ReplaceLength:
		result.Action, result.Reason = "degraded", "收到降级长度的状态，未保存；下一次将按出口池重试规则继续"
	default:
		result.Action, result.Reason = "error", "响应未包含符合模板长度的 turn-state"
	}
	accountCooldownKey := accountKey(c.account)
	oldCooldown, hadCooldown := m.state.Cooldowns[accountCooldownKey]
	m.state.Cooldowns[accountCooldownKey] = cooldown{Until: now.Add(rest)}
	oldExitCooldown := m.state.Cooldowns[c.cooldownKey]
	if result.Action == "harvested" {
		// A successful exit may be reused when its template needs renewal.
		// Keeping the failure budget of 55 minutes would outlive short TTLs,
		// and even the default TTL when the returned template is already old.
		t := m.state.Templates[key(c.account, c.model)]
		renewAt := t.IssuedAt.Add(time.Duration(m.state.Config.TTLSeconds)*time.Second - renewalLead(m.state.Config.TTLSeconds))
		m.state.Cooldowns[c.cooldownKey] = cooldown{Until: renewAt}
	}
	// next_check_at is a global scheduling hint, not necessarily this account's
	// cooldown: another saved account can be probed by the next bounded call.
	if err := m.persistLocked(); err != nil {
		m.state.Cooldowns[c.cooldownKey] = oldExitCooldown
		if hadCooldown {
			m.state.Cooldowns[accountCooldownKey] = oldCooldown
		} else {
			delete(m.state.Cooldowns, accountCooldownKey)
		}
		return ProbeResult{}, err
	}
	return result, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ParseCredential reads only OAuth access tokens. API keys are not supported;
// the probe never refreshes or writes credentials back to the host.
func ParseCredential(raw []byte, now time.Time) (Credential, error) {
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		return Credential{}, errors.New("认证文件格式无效")
	}
	token, _ := file["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return Credential{}, errors.New("认证文件没有 OAuth access_token")
	}
	account, _ := file["account_id"].(string)
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		if json.Unmarshal(claimsRaw, &claims) == nil {
			if expiration, ok := claims["exp"].(float64); ok && expiration <= float64(now.Unix()) {
				return Credential{}, errors.New("OAuth access_token 已过期，请等待 CPA 刷新或重新登录")
			}
			if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
				if id, _ := auth["chatgpt_account_id"].(string); id != "" {
					account = id
				}
			}
		}
	}
	if strings.ContainsAny(account, "\r\n\x00") {
		return Credential{}, errors.New("认证文件账号无效")
	}
	return Credential{AccessToken: token, AccountID: account}, nil
}

func curlQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\v", "\\v")
	return "\"" + replacer.Replace(value) + "\""
}

func buildCurlConfig(credential Credential, model, proxy string) string {
	if strings.HasPrefix(proxy, "socks5://") {
		proxy = "socks5h://" + strings.TrimPrefix(proxy, "socks5://")
	}
	payload, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "store": false, "instructions": "",
		"input":     []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply OK."}}}},
		"reasoning": map[string]string{"effort": "low"}, "parallel_tool_calls": false,
	})
	var session [16]byte
	_, _ = rand.Read(session[:])
	lines := []string{
		"url = \"https://chatgpt.com/backend-api/codex/responses\"", "request = \"POST\"",
		"proxy = " + curlQuote(proxy), "noproxy = \"\"", "connect-timeout = 10", "max-time = 25",
		"silent", "dump-header = \"-\"", "output = " + curlQuote(os.DevNull),
		"write-out = \"\\nHTTP_STATUS:%{http_code}\\n\"", "proto = \"=https\"",
		"header = " + curlQuote("Authorization: Bearer "+credential.AccessToken),
		"header = \"Content-Type: application/json\"", "header = \"Accept: text/event-stream\"",
		"header = \"Originator: codex-tui\"", "header = " + curlQuote(fmt.Sprintf("Session-Id: %x", session)),
		"user-agent = \"codex-tui/0.154.0 (Linux; x86_64)\"",
		"data-binary = " + curlQuote(string(payload)),
	}
	if credential.AccountID != "" {
		lines = append(lines, "header = "+curlQuote("Chatgpt-Account-Id: "+credential.AccountID))
	}
	return strings.Join(lines, "\n") + "\n"
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < b.limit {
		keep := b.limit - b.Len()
		if keep > len(p) {
			keep = len(p)
		}
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}

func runCurlProbe(credential Credential, model, proxy string) (ProbeResponse, error) {
	// -q first disables the user's .curlrc. All sensitive arguments go through
	// stdin; command-line listings contain no OAuth token or proxy password.
	cmd := exec.Command("curl", "-q", "--config", "-")
	cmd.Stdin = strings.NewReader(buildCurlConfig(credential, model, proxy))
	output := &boundedBuffer{limit: 65536}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	err := cmd.Run()
	result := parseCurlHeaders(output.String())
	// curl can time out while draining a successful SSE response after receiving
	// all headers. A valid 200 header is sufficient to evaluate this probe.
	if err != nil && result.Status == 0 {
		return result, errors.New("curl 请求失败")
	}
	return result, nil
}

func parseCurlHeaders(raw string) ProbeResponse {
	result := ProbeResponse{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				status, _ := strconv.Atoi(parts[1])
				// Reset on each response block (proxy CONNECT, 100, then upstream).
				result = ProbeResponse{Status: status}
			}
		} else if colon := strings.IndexByte(line, ':'); colon > 0 && strings.EqualFold(line[:colon], Header) {
			result.Value = strings.TrimSpace(line[colon+1:])
		} else if strings.HasPrefix(line, "HTTP_STATUS:") {
			if status, _ := strconv.Atoi(strings.TrimPrefix(line, "HTTP_STATUS:")); status != 0 {
				result.Status = status
				return result
			}
			return ProbeResponse{}
		}
	}
	// Without curl's final status marker even a 200 can belong to a proxy's
	// CONNECT handshake rather than a successfully negotiated upstream request.
	return ProbeResponse{}
}
