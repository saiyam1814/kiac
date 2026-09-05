package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
)

func TestUploadRoundTrip(t *testing.T) {
	server := httptest.NewServer(uploadHandler())
	defer server.Close()
	for _, size := range []int{1 << 20, 32 << 20} {
		if err := upload(server.URL+"/upload", size); err != nil {
			t.Fatalf("%d-byte upload: %v", size, err)
		}
	}
}

func TestPayloadIsDeterministic(t *testing.T) {
	payload, err := io.ReadAll(newTestPayloadReader(1024))
	if err != nil {
		t.Fatal(err)
	}
	got := sha256.Sum256(payload)
	const want = "ff29997907aed0fcb4456a2e4b450fa978bfbcae5e79678279cfd70f928b03b1"
	if fmt.Sprintf("%x", got) != want {
		t.Fatalf("payload hash = %x, want %s", got, want)
	}
}

func TestPayloadReaderStreamsWithoutOverrun(t *testing.T) {
	reader := newTestPayloadReader(17)
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 17 {
		t.Fatalf("payload length = %d, want 17", len(payload))
	}
	buffer := make([]byte, 8)
	if n, err := reader.Read(buffer); n != 0 || err != io.EOF {
		t.Fatalf("read after end = %d, %v; want 0, EOF", n, err)
	}
}
