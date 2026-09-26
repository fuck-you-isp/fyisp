package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// quickTunnel mirrors the result of POST <quick-service>/tunnel as parsed by
// cloudflared's cmd/cloudflared/tunnel/quick_tunnel.go (RunQuickTunnel).
type quickTunnel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	AccountTag string `json:"account_tag"`
	Secret     []byte `json:"secret"`

	tunnelID uuid.UUID
}

type quickTunnelResponse struct {
	Success bool
	Result  quickTunnel
	Errors  []struct {
		Code    int
		Message string
	}
}

type rateLimitedError struct {
	retryAfter time.Duration
}

func (e *rateLimitedError) Error() string {
	return "quick tunnel service rate limited this client (HTTP 429)"
}

const provisionTimeout = 15 * time.Second

// provision requests a new account-less quick tunnel.
func provision(ctx context.Context, service string) (*quickTunnel, error) {
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(service, "/")+"/tunnel", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cloudflared/"+CloudflaredVersion)
	// No keep-alive: this runs once per URL and must not leave goroutines behind.
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   provisionTimeout,
		ResponseHeaderTimeout: provisionTimeout,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request quick tunnel: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read quick tunnel response: %w", err)
	}
	return parseProvisionResponse(resp.StatusCode, resp.Header, body)
}

func parseProvisionResponse(status int, h http.Header, body []byte) (*quickTunnel, error) {
	if status == http.StatusTooManyRequests {
		e := &rateLimitedError{}
		if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
			e.retryAfter = time.Duration(s) * time.Second
		}
		return nil, e
	}
	var data quickTunnelResponse
	jsonErr := json.Unmarshal(body, &data)
	if len(data.Errors) > 0 {
		msgs := make([]string, len(data.Errors))
		for i, e := range data.Errors {
			msgs[i] = fmt.Sprintf("[%d] %s", e.Code, e.Message)
		}
		return nil, fmt.Errorf("quick tunnel provisioning failed (HTTP %d): %s", status, strings.Join(msgs, "; "))
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("quick tunnel provisioning failed with HTTP %d", status)
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("decode quick tunnel response: %w", jsonErr)
	}
	if !data.Success {
		return nil, errors.New("quick tunnel provisioning failed")
	}
	qt := data.Result
	id, err := uuid.Parse(qt.ID)
	if err != nil {
		return nil, fmt.Errorf("quick tunnel ID: %w", err)
	}
	qt.tunnelID = id
	qt.Hostname = strings.TrimPrefix(qt.Hostname, "https://")
	if qt.Hostname == "" || qt.AccountTag == "" || len(qt.Secret) == 0 {
		return nil, errors.New("quick tunnel response is missing hostname, account or secret")
	}
	return &qt, nil
}
