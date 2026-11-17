package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type nativeLeaseRequest struct {
	Namespace  string `json:"namespace"`
	ClaimUID   string `json:"claimUID"`
	NodeName   string `json:"nodeName"`
	TTLSeconds int    `json:"ttlSeconds"`
}

type nativeLease struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type nativeBrokerHealth struct {
	Ready              bool   `json:"ready"`
	Backend            string `json:"backend"`
	InferenceAvailable bool   `json:"inferenceAvailable"`
}

type nativeBroker interface {
	Health(context.Context) (nativeBrokerHealth, error)
	Acquire(context.Context, nativeLeaseRequest) (nativeLease, error)
	Revoke(context.Context, string) error
}

type nativeBrokerClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func newNativeBrokerClient(endpoint, caFile, tokenFile string) (*nativeBrokerClient, []byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, nil, errors.New("--native-endpoint must be an HTTPS origin without credentials, path, query or fragment")
	}
	ca, err := readNativeFile(caFile, 64<<10)
	if err != nil {
		return nil, nil, fmt.Errorf("read native broker CA: %w", err)
	}
	pool := x509.NewCertPool()
	var publicCA bytes.Buffer
	for rest := ca; len(rest) != 0; {
		block, tail := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = tail
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, nil, errors.New("native broker CA contains an invalid certificate")
		}
		_ = pem.Encode(&publicCA, block)
	}
	ca = publicCA.Bytes()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, nil, errors.New("native broker CA contains no valid certificates")
	}
	rawToken, err := readNativeFile(tokenFile, 4096)
	if err != nil {
		return nil, nil, fmt.Errorf("read native broker admin token: %w", err)
	}
	token := strings.TrimSpace(string(rawToken))
	if !validNativeToken(token) {
		return nil, nil, errors.New("native broker admin token must contain 16 to 4096 visible ASCII characters without whitespace")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	return &nativeBrokerClient{
		endpoint: strings.TrimSuffix(endpoint, "/"), token: token,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("native broker redirects are not allowed")
		}},
	}, ca, nil
}

func readNativeFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return raw, nil
}

func validNativeToken(token string) bool {
	if len(token) < 16 || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

func (c *nativeBrokerClient) request(ctx context.Context, method, path string, body any, out any) error {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("native broker request failed: %w", err)
	}
	defer response.Body.Close()
	accepted := (method == http.MethodGet && response.StatusCode == http.StatusOK) ||
		(method == http.MethodPost && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated)) ||
		(method == http.MethodDelete && response.StatusCode == http.StatusNoContent)
	if !accepted {
		// Never include response bodies: a misconfigured endpoint could echo secrets.
		return fmt.Errorf("native broker returned HTTP %d", response.StatusCode)
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil {
		return fmt.Errorf("read native broker response: %w", err)
	}
	if len(raw) > 16<<10 {
		return errors.New("native broker response exceeds 16 KiB")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("native broker returned invalid JSON")
	}
	return nil
}

func (c *nativeBrokerClient) Health(ctx context.Context) (nativeBrokerHealth, error) {
	var health nativeBrokerHealth
	if err := c.request(ctx, http.MethodGet, "/v1/admin/health", nil, &health); err != nil {
		return health, err
	}
	if !health.Ready || health.Backend != "native-metal" {
		return health, errors.New("native Metal broker is not ready")
	}
	return health, nil
}

func (c *nativeBrokerClient) Acquire(ctx context.Context, request nativeLeaseRequest) (nativeLease, error) {
	var lease nativeLease
	if err := c.request(ctx, http.MethodPost, "/v1/leases", request, &lease); err != nil {
		return lease, err
	}
	if err := validateNativeLease(lease, time.Now(), time.Duration(request.TTLSeconds)*time.Second); err != nil {
		return nativeLease{}, err
	}
	return lease, nil
}

func validateNativeLease(lease nativeLease, now time.Time, ttl time.Duration) error {
	if lease.ID == "" || len(lease.ID) > 128 {
		return errors.New("native broker returned an invalid lease ID")
	}
	for _, c := range lease.ID {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return errors.New("native broker returned an invalid lease ID")
		}
	}
	if !validNativeToken(lease.Token) {
		return errors.New("native broker returned an invalid lease token")
	}
	if !lease.ExpiresAt.After(now.Add(5*time.Second)) || lease.ExpiresAt.After(now.Add(ttl+10*time.Second)) {
		return errors.New("native broker returned an invalid lease expiry")
	}
	return nil
}

func (c *nativeBrokerClient) Revoke(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return errors.New("invalid native lease ID")
	}
	return c.request(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(id), nil, nil)
}
