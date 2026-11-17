// Package nativegpu implements the opt-in host service for native Apple GPU
// workloads. Linux clients use an authenticated RPC API, not a Metal device.
package nativegpu

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxRequestBytes = 32 << 10

type LeaseRequest struct {
	Namespace  string `json:"namespace"`
	ClaimUID   string `json:"claimUID"`
	NodeName   string `json:"nodeName"`
	TTLSeconds int    `json:"ttlSeconds"`
}

type Lease struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type activeLease struct {
	Lease
	identity LeaseRequest
	ctx      context.Context
	cancel   context.CancelFunc
	timer    *time.Timer
}

// Broker admits one exclusive host-service lease across all attached clusters.
// This controls Kiac-owned work; other macOS applications still share the GPU.
type Broker struct {
	engine       Engine
	adminToken   string
	capabilities json.RawMessage
	inference    bool
	mu           sync.Mutex
	lease        *activeLease
	closed       bool
	busy         chan struct{}
}

func NewBroker(engine Engine, adminToken string, capabilities json.RawMessage, inference bool) (*Broker, error) {
	if engine == nil || len(adminToken) < 32 || !json.Valid(capabilities) {
		return nil, errors.New("native GPU broker requires an engine, a private admin token, and valid capabilities")
	}
	return &Broker{engine: engine, adminToken: adminToken, capabilities: bytes.Clone(capabilities), inference: inference, busy: make(chan struct{}, 1)}, nil
}

func secret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func authorized(header, token string) bool {
	return subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

var identityPart = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$`)

func (b *Broker) issue(request LeaseRequest) (Lease, bool, error) {
	if !identityPart.MatchString(request.Namespace) || !identityPart.MatchString(request.ClaimUID) || !identityPart.MatchString(request.NodeName) {
		return Lease{}, false, errors.New("namespace, claimUID and nodeName must be nonempty Kubernetes identifiers")
	}
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 120
	}
	if request.TTLSeconds < 30 || request.TTLSeconds > 300 {
		return Lease{}, false, errors.New("ttlSeconds must be between 30 and 300")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Lease{}, false, errUnavailable
	}
	if current := b.lease; current != nil {
		if !time.Now().Before(current.ExpiresAt) {
			b.clearLocked()
		} else if current.identity.Namespace == request.Namespace && current.identity.ClaimUID == request.ClaimUID && current.identity.NodeName == request.NodeName {
			current.ExpiresAt = time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)
			current.timer.Reset(time.Until(current.ExpiresAt))
			return current.Lease, false, nil
		} else {
			return Lease{}, false, errOccupied
		}
	}
	id, err := secret()
	if err != nil {
		return Lease{}, false, err
	}
	token, err := secret()
	if err != nil {
		return Lease{}, false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	current := &activeLease{Lease: Lease{ID: id, Token: token, ExpiresAt: time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)}, identity: request, ctx: ctx, cancel: cancel}
	current.timer = time.AfterFunc(time.Until(current.ExpiresAt), func() { b.expire(id) })
	b.lease = current
	return current.Lease, true, nil
}

var (
	errOccupied    = errors.New("the native GPU host service already has an exclusive lease")
	errUnavailable = errors.New("native GPU service is shutting down")
)

func (b *Broker) clearLocked() {
	if b.lease != nil {
		b.lease.timer.Stop()
		b.lease.cancel()
		b.lease = nil
	}
}

func (b *Broker) expire(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lease == nil || b.lease.ID != id {
		return
	}
	if remaining := time.Until(b.lease.ExpiresAt); remaining > 0 {
		b.lease.timer.Reset(remaining)
		return
	}
	b.clearLocked()
}

func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.clearLocked()
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Browsers are not a supported client; avoid exposing local credentials to
	// cross-origin pages or browser-driven requests to an opted-in VM interface.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser origins are not allowed", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/admin/") || r.URL.Path == "/v1/leases" || strings.HasPrefix(r.URL.Path, "/v1/leases/") {
		if !authorized(r.Header.Get("Authorization"), b.adminToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		b.admin(w, r)
		return
	}
	b.mu.Lock()
	current := b.lease
	valid := current != nil && time.Now().Before(current.ExpiresAt) && authorized(r.Header.Get("Authorization"), current.Token) && !b.closed
	b.mu.Unlock()
	if !valid {
		http.Error(w, "a current native GPU lease is required", http.StatusUnauthorized)
		return
	}
	select {
	case b.busy <- struct{}{}:
		defer func() { <-b.busy }()
	default:
		http.Error(w, "native GPU worker busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	stop := context.AfterFunc(current.ctx, cancel)
	defer stop()
	defer cancel()
	if current.ctx.Err() != nil {
		http.Error(w, "native GPU lease expired or was revoked", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities":
		if health, ok := b.engine.(interface{ Health(context.Context) error }); ok {
			if health.Health(ctx) != nil {
				http.Error(w, "native GPU engine is unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		capabilities, err := b.engine.Capabilities(ctx)
		if err != nil || !json.Valid(capabilities) {
			http.Error(w, "native GPU capabilities are unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(capabilities)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/compute/vector-add":
		var request struct {
			Count int `json:"count"`
		}
		if decodeRequest(w, r, &request) != nil || request.Count < 1 || request.Count > 65536 {
			http.Error(w, "count must be an integer from 1 to 65536", http.StatusBadRequest)
			return
		}
		output, err := b.engine.SelfTest(ctx, request.Count)
		if err != nil || !json.Valid(output) {
			http.Error(w, "native Metal self-test failed", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(output)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		b.chat(ctx, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (b *Broker) admin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/health":
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed {
			http.Error(w, "service stopping", http.StatusServiceUnavailable)
			return
		}
		if health, ok := b.engine.(interface{ Health(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if health.Health(ctx) != nil {
				http.Error(w, "native GPU engine is unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "backend": "native-metal", "inferenceAvailable": b.inference, "capabilities": b.capabilities})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
		var request LeaseRequest
		if decodeRequest(w, r, &request) != nil {
			http.Error(w, "invalid lease request", http.StatusBadRequest)
			return
		}
		lease, created, err := b.issue(request)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errOccupied) {
				status = http.StatusConflict
			}
			if errors.Is(err, errUnavailable) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, lease)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/leases/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
		b.mu.Lock()
		if b.lease != nil && b.lease.ID == id {
			b.clearLocked()
		}
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

type ChatRequest struct {
	Model       string        `json:"model,omitempty"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (b *Broker) chat(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if !b.inference {
		http.Error(w, "start the native service with a local GGUF model to enable inference", http.StatusServiceUnavailable)
		return
	}
	var request ChatRequest
	if decodeRequest(w, r, &request) != nil {
		http.Error(w, "unsupported or invalid chat request", http.StatusBadRequest)
		return
	}
	if request.MaxTokens == nil {
		value := 64
		request.MaxTokens = &value
	}
	valid := len(request.Messages) > 0 && len(request.Messages) <= 16 && *request.MaxTokens >= 1 && *request.MaxTokens <= 512 && (request.Model == "" || request.Model == "kiac-native")
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		valid = false
	}
	for _, message := range request.Messages {
		if (message.Role != "user" && message.Role != "system" && message.Role != "assistant") || len(message.Content) == 0 || len(message.Content) > 8192 {
			valid = false
		}
	}
	if !valid {
		http.Error(w, "chat requires 1-16 text messages, model kiac-native and max_tokens 1-512", http.StatusBadRequest)
		return
	}
	request.Model = "kiac-native"
	body, _ := json.Marshal(request)
	response, err := b.engine.Chat(ctx, body)
	if err != nil {
		http.Error(w, "native inference engine is unavailable", http.StatusServiceUnavailable)
		return
	}
	defer response.Body.Close()
	contentType := response.Header.Get("Content-Type")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("native inference rejected the request (HTTP %d)", response.StatusCode), http.StatusBadGateway)
		return
	}
	if !strings.HasPrefix(contentType, "application/json") && !strings.HasPrefix(contentType, "text/event-stream") {
		http.Error(w, "invalid native inference response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	buffer := make([]byte, 16<<10)
	limited := io.LimitReader(response.Body, 4<<20)
	for {
		n, err := limited.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
