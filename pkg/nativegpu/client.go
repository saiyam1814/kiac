package nativegpu

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials is mounted read-only by the native DRA plugin. Clients reload it
// for every operation so renewal and broker restart can replace credentials.
type Credentials struct {
	Version   int       `json:"version"`
	Endpoint  string    `json:"endpoint"`
	LeaseID   string    `json:"leaseID,omitempty"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	CAFile    string    `json:"caFile"`
}

func ReadCredentials(path string) (Credentials, error) {
	var credentials Credentials
	info, err := os.Stat(path)
	if err != nil {
		return credentials, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 {
		return credentials, errors.New("invalid native GPU credentials file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return credentials, err
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return credentials, errors.New("invalid native GPU credentials JSON")
	}
	if credentials.Version != 1 || len(credentials.Token) < 32 {
		return credentials, errors.New("unsupported or incomplete native GPU credentials")
	}
	if !filepath.IsAbs(credentials.CAFile) {
		credentials.CAFile = filepath.Join(filepath.Dir(path), credentials.CAFile)
	}
	return credentials, nil
}

func AdminCredentials(dir string) (Credentials, error) {
	var info struct {
		Endpoint string `json:"endpoint"`
	}
	data, err := readPrivate(filepath.Join(dir, "service.json"), 16384)
	if err != nil {
		return Credentials{}, fmt.Errorf("native GPU service is not running: %w", err)
	}
	if json.Unmarshal(data, &info) != nil {
		return Credentials{}, errors.New("invalid native GPU service state")
	}
	token, err := readPrivate(filepath.Join(dir, "admin.token"), 4096)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Version: 1, Endpoint: info.Endpoint, Token: strings.TrimSpace(string(token)), CAFile: filepath.Join(dir, "ca.crt")}, nil
}

func Client(credentials Credentials) (*http.Client, error) {
	endpoint, err := url.Parse(credentials.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("native GPU endpoint must be an HTTPS origin")
	}
	info, err := os.Stat(credentials.CAFile)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, errors.New("invalid native GPU CA file")
	}
	data, err := os.ReadFile(credentials.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid native GPU CA certificate")
	}
	return &http.Client{Timeout: 125 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}, ResponseHeaderTimeout: 120 * time.Second, IdleConnTimeout: 10 * time.Second}}, nil
}

func Request(ctx context.Context, credentials Credentials, method, path string, body any) (*http.Response, error) {
	if !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "?#") {
		return nil, errors.New("invalid native GPU API path")
	}
	client, err := Client(credentials)
	if err != nil {
		return nil, err
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, credentials.Endpoint+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credentials.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		client.CloseIdleConnections()
		return nil, err
	}
	response.Body = &clientBody{ReadCloser: response.Body, client: client}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("native GPU API HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return response, nil
}

type clientBody struct {
	io.ReadCloser
	client *http.Client
}

func (b *clientBody) Close() error {
	err := b.ReadCloser.Close()
	b.client.CloseIdleConnections()
	return err
}
