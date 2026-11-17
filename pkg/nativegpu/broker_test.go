package nativegpu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type brokerEngine struct {
	self      func(context.Context, int) (json.RawMessage, error)
	chat      func(context.Context, json.RawMessage) (*http.Response, error)
	unhealthy bool
}

func (*brokerEngine) Capabilities(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"backend":"native-metal"}`), nil
}
func (e *brokerEngine) SelfTest(ctx context.Context, n int) (json.RawMessage, error) {
	if e.self != nil {
		return e.self(ctx, n)
	}
	return json.RawMessage(`{"verified":true}`), nil
}
func (e *brokerEngine) Chat(ctx context.Context, b json.RawMessage) (*http.Response, error) {
	if e.chat != nil {
		return e.chat(ctx, b)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"test"}}]}`))}, nil
}
func (*brokerEngine) Close() error { return nil }
func (e *brokerEngine) Health(context.Context) error {
	if e.unhealthy {
		return errors.New("offline")
	}
	return nil
}

const adminSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testBroker(t *testing.T, e *brokerEngine) *Broker {
	t.Helper()
	b, err := NewBroker(e, adminSecret, json.RawMessage(`{"backend":"native-metal"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}
func invoke(b *Broker, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}
func leaseFor(t *testing.T, b *Broker) Lease {
	t.Helper()
	lease, _, err := b.issue(LeaseRequest{Namespace: "demo", ClaimUID: "claim-123", NodeName: "worker-1", TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestBrokerAuthorizationAndLeaseIsolation(t *testing.T) {
	b := testBroker(t, &brokerEngine{})
	for _, path := range []string{"/v1/admin/health", "/v1/capabilities"} {
		if w := invoke(b, "GET", path, "", ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	lease := leaseFor(t, b)
	if w := invoke(b, "GET", "/v1/admin/health", lease.Token, ""); w.Code != 401 {
		t.Fatalf("lease accessed admin: %d", w.Code)
	}
	if w := invoke(b, "GET", "/v1/capabilities", adminSecret, ""); w.Code != 401 {
		t.Fatalf("admin credential used as workload token: %d", w.Code)
	}
	if w := invoke(b, "GET", "/v1/capabilities", lease.Token, ""); w.Code != 200 {
		t.Fatalf("capabilities: %d", w.Code)
	}
	request := LeaseRequest{Namespace: "demo", ClaimUID: "claim-123", NodeName: "worker-1", TTLSeconds: 300}
	renewed, created, err := b.issue(request)
	if err != nil || created || renewed.ID != lease.ID || renewed.Token != lease.Token || !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatal("renewal changed live credentials or failed", err)
	}
	request.NodeName = "worker-2"
	if _, _, err := b.issue(request); !errors.Is(err, errOccupied) {
		t.Fatal("second node acquired same host GPU", err)
	}
	request.NodeName = "worker-1"
	request.Namespace = "different"
	if _, _, err := b.issue(request); !errors.Is(err, errOccupied) {
		t.Fatal("second namespace acquired same host GPU", err)
	}
	if w := invoke(b, "DELETE", "/v1/leases/"+lease.ID, adminSecret, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := invoke(b, "DELETE", "/v1/leases/"+lease.ID, adminSecret, ""); w.Code != 204 {
		t.Fatal("revoke not idempotent", w.Code)
	}
	if w := invoke(b, "GET", "/v1/capabilities", lease.Token, ""); w.Code != 401 {
		t.Fatal("revoked token accepted", w.Code)
	}
}

func TestBrokerExpiryAndRestartFailClosed(t *testing.T) {
	b := testBroker(t, &brokerEngine{})
	lease := leaseFor(t, b)
	b.mu.Lock()
	b.lease.ExpiresAt = time.Now().Add(-time.Second)
	b.mu.Unlock()
	b.expire(lease.ID)
	if w := invoke(b, "GET", "/v1/capabilities", lease.Token, ""); w.Code != 401 {
		t.Fatal("expired token accepted")
	}
	other := testBroker(t, &brokerEngine{})
	if w := invoke(other, "GET", "/v1/capabilities", lease.Token, ""); w.Code != 401 {
		t.Fatal("old token survived a broker restart")
	}
	if _, _, err := b.issue(LeaseRequest{Namespace: "x", ClaimUID: "y", NodeName: "z", TTLSeconds: 1}); err == nil {
		t.Fatal("unbounded/short TTL accepted")
	}
	b.Close()
	if _, _, err := b.issue(LeaseRequest{Namespace: "x", ClaimUID: "y", NodeName: "z"}); !errors.Is(err, errUnavailable) {
		t.Fatal("closed broker granted lease")
	}
}

func TestBrokerRevocationCancelsActiveWorkAndBoundsConcurrency(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	e := &brokerEngine{self: func(ctx context.Context, _ int) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}}
	b := testBroker(t, e)
	lease := leaseFor(t, b)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- invoke(b, "POST", "/v1/compute/vector-add", lease.Token, `{"count":17}`) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if w := invoke(b, "POST", "/v1/compute/vector-add", lease.Token, `{"count":17}`); w.Code != 429 {
		t.Fatal("concurrent GPU operation accepted", w.Code)
	}
	invoke(b, "DELETE", "/v1/leases/"+lease.ID, adminSecret, "")
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel active compute")
	}
	select {
	case w := <-done:
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request did not complete")
	}
}

func TestBrokerRejectsUnsupportedWorkAndUnhealthyEngine(t *testing.T) {
	e := &brokerEngine{}
	b := testBroker(t, e)
	lease := leaseFor(t, b)
	for _, body := range []string{`{"count":0}`, `{"count":true}`, `{"count":1.5}`, `{"count":65537}`, `{"count":17,"shader":"arbitrary"}`, `{"count":17} {}`} {
		if w := invoke(b, "POST", "/v1/compute/vector-add", lease.Token, body); w.Code != 400 {
			t.Fatalf("accepted %s (%d)", body, w.Code)
		}
	}
	if w := invoke(b, "POST", "/v1/compute/vector-add", lease.Token, `{"count":17,"x":"`+strings.Repeat("a", MaxRequestBytes)+`"}`); w.Code < 400 {
		t.Fatal("oversized body accepted")
	}
	for _, body := range []string{`{"messages":[]}`, `{"messages":[{"role":"tool","content":"x"}]}`, `{"messages":[{"role":"user","content":"x"}],"max_tokens":999}`, `{"messages":[{"role":"user","content":"x"}],"tools":[]}`, `{"model":"remote","messages":[{"role":"user","content":"x"}]}`} {
		if w := invoke(b, "POST", "/v1/chat/completions", lease.Token, body); w.Code != 400 {
			t.Fatalf("accepted chat %s (%d)", body, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/v1/capabilities", nil)
	r.Header.Set("Authorization", "Bearer "+lease.Token)
	r.Header.Set("Origin", "http://hostile.example")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("browser origin accepted")
	}
	e.unhealthy = true
	if w := invoke(b, "GET", "/v1/capabilities", lease.Token, ""); w.Code != 503 {
		t.Fatal("dead native engine advertised cached capabilities", w.Code)
	}
	if w := invoke(b, "GET", "/v1/admin/health", adminSecret, ""); w.Code != 503 {
		t.Fatal("dead native engine advertised ready", w.Code)
	}
}

func TestBrokerForwardsTextAndStreamingWithoutCredentials(t *testing.T) {
	e := &brokerEngine{chat: func(_ context.Context, body json.RawMessage) (*http.Response, error) {
		var request ChatRequest
		if json.Unmarshal(body, &request) != nil || request.Model != "kiac-native" || *request.MaxTokens != 64 {
			t.Fatal("invalid normalized request", string(body))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Authorization": []string{"private-engine-key"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))}, nil
	}}
	b := testBroker(t, e)
	lease := leaseFor(t, b)
	w := invoke(b, "POST", "/v1/chat/completions", lease.Token, `{"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") || w.Header().Get("Authorization") != "" || !w.Flushed {
		t.Fatal("stream was not safely forwarded", w.Code, w.Body.String())
	}
}
