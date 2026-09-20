// Package statecollector drives the plugin's synchronous server-collection
// endpoint from a separate process. It never handles upstream credentials or
// reads plugin state files; scheduling and ownership remain with the plugin.
package statecollector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	RunnerPath = "/v0/management/plugins/cpa-team-manager/turn-state/runner"
	DefaultURL = "http://127.0.0.1:8317"
	maxBody    = 2 << 20
	maxKey     = 16 << 10
)

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)

type Config struct {
	URL               string
	ManagementKeyFile string
	Version           string
	Logger            *log.Logger
}

// Status contains only non-sensitive scheduling fields. Error text, probe
// events, account identities, and response bodies are deliberately ignored.
type Status struct {
	Enabled         bool    `json:"enabled"`
	Phase           string  `json:"phase"`
	Online          bool    `json:"online"`
	NextTickSeconds float64 `json:"next_tick_seconds"`
}

type Collector struct {
	baseURL    string
	keyFile    string
	version    string
	instanceID string
	client     *http.Client
	logger     *log.Logger
	wait       func(context.Context, time.Duration) error
	lookupKey  func() string
}

// New validates configuration without contacting the host. A key file takes
// precedence over CPA_MANAGEMENT_KEY and is reread before every request, so a
// service can recover after operators rotate or replace its credential file.
func New(cfg Config) (*Collector, error) {
	base := cfg.URL
	if strings.TrimSpace(base) == "" {
		base = DefaultURL
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("management URL must be an HTTP(S) origin without credentials, path, query, or fragment")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("management URL has an invalid port")
		}
	}
	u.Path = ""
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return nil, errors.New("could not create collector instance identity")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = "dev"
	}
	if !versionPattern.MatchString(version) {
		return nil, errors.New("collector version is invalid")
	}
	transport := &http.Transport{
		// nil Proxy is intentional: environment proxy variables must never
		// forward the CPA management credential to a different endpoint.
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  40 * time.Second,
		IdleConnTimeout:        60 * time.Second,
		MaxIdleConnsPerHost:    1,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
	collector := &Collector{
		baseURL: u.String(), keyFile: cfg.ManagementKeyFile, version: version,
		instanceID: hex.EncodeToString(identity[:]), logger: logger, wait: waitContext,
		lookupKey: func() string { return os.Getenv("CPA_MANAGEMENT_KEY") },
		client: &http.Client{Transport: transport, Timeout: 45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		logger.Print("warning: management URL uses HTTP outside loopback; use a trusted private network or HTTPS")
	}
	return collector, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Collector) Close() { c.client.CloseIdleConnections() }

// Check verifies connectivity/authentication through a read-only GET. It does
// not claim a runner lease, enable collection, or trigger an upstream request.
func (c *Collector) Check(ctx context.Context) (Status, error) {
	status, _, err := c.call(ctx, http.MethodGet, RunnerPath)
	return status, err
}

// Run sends one tick at a time. The plugin owns desired state, due times,
// account/exit cooldowns, and the single-runner lease. Even while collection is
// stopped, ticks keep this external process visible as available to the UI.
func (c *Collector) Run(ctx context.Context) error {
	defer c.Close()
	backoff := 3 * time.Second
	lastObservation := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, busy, err := c.call(ctx, http.MethodPost, RunnerPath+"/tick")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := 5 * time.Second
		observation := ""
		switch {
		case busy:
			observation = "another collector owns the lease or a tick is still running"
			backoff = 3 * time.Second
		case err != nil:
			observation = err.Error()
			delay = backoff
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		default:
			observation = "collection enabled=" + strconv.FormatBool(status.Enabled) + " phase=" + safePhase(status.Phase)
			delay = tickDelay(status.NextTickSeconds)
			backoff = 3 * time.Second
		}
		if observation != lastObservation {
			c.logger.Print(observation)
			lastObservation = observation
		}
		if err := c.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func tickDelay(seconds float64) time.Duration {
	if seconds <= 0 {
		return 5 * time.Second
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds * float64(time.Second))
}

func safePhase(phase string) string {
	switch phase {
	case "stopped", "waiting", "running", "draining", "offline":
		return phase
	default:
		return "unknown"
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Collector) managementKey() (string, error) {
	key := ""
	if c.keyFile != "" {
		file, err := os.Open(c.keyFile)
		if err != nil {
			return "", errors.New("management key file is unavailable; retrying")
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, maxKey+1))
		_ = file.Close()
		if readErr != nil || len(raw) > maxKey {
			return "", errors.New("management key file could not be read safely; retrying")
		}
		key = strings.TrimSpace(string(raw))
	} else {
		key = strings.TrimSpace(c.lookupKey())
	}
	if key == "" || len(key) > maxKey || strings.ContainsAny(key, "\r\n\x00") {
		return "", errors.New("management key is missing or invalid; set a key file or CPA_MANAGEMENT_KEY")
	}
	return key, nil
}

// Errors deliberately contain only fixed text and HTTP status numbers. Neither
// transport errors (which may contain URLs) nor host response bodies are logged.
func (c *Collector) call(ctx context.Context, method, path string) (Status, bool, error) {
	key, err := c.managementKey()
	if err != nil {
		return Status{}, false, err
	}
	var body io.Reader
	if method == http.MethodPost {
		raw, _ := json.Marshal(struct {
			InstanceID string `json:"instance_id"`
			Version    string `json:"version"`
		}{c.instanceID, c.version})
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return Status{}, false, errors.New("could not prepare management request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return Status{}, false, errors.New("management connection failed or timed out; retrying")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict && method == http.MethodPost {
		return Status{}, true, nil
	}
	if response.StatusCode != http.StatusOK {
		return Status{}, false, errors.New("management endpoint returned HTTP " + strconv.Itoa(response.StatusCode) + "; retrying")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return Status{}, false, errors.New("management response could not be read safely; retrying")
	}
	var result struct {
		Enabled         *bool   `json:"enabled"`
		Phase           string  `json:"phase"`
		Online          bool    `json:"online"`
		NextTickSeconds float64 `json:"next_tick_seconds"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Enabled == nil || safePhase(result.Phase) == "unknown" {
		return Status{}, false, errors.New("management response has an invalid runner status; retrying")
	}
	return Status{Enabled: *result.Enabled, Phase: result.Phase, Online: result.Online, NextTickSeconds: result.NextTickSeconds}, false, nil
}
