package main

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestUploadRoundTrip(t *testing.T) {
	server := httptest.NewServer(uploadHandler())
	defer server.Close()
	if err := upload(server.URL+"/upload", 1<<20); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadIsDeterministic(t *testing.T) {
	got := sha256.Sum256(testPayload(1024))
	const want = "ff29997907aed0fcb4456a2e4b450fa978bfbcae5e79678279cfd70f928b03b1"
	if fmt.Sprintf("%x", got) != want {
		t.Fatalf("payload hash = %x, want %s", got, want)
	}
}
