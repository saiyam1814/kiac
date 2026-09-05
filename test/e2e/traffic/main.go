// Command traffic provides the tiny HTTP server and client used by the
// runtime smoke suite. It is copied into a pod and sibling node VM so the
// test does not depend on curl, Python, or other tools in node images.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const tunnelProtocol = "KIACEDGE/2"

const maxUploadBytes = 64 << 20

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: traffic <server|upload|reject>")
	}
	switch args[0] {
	case "server":
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		listen := fs.String("listen", ":8080", "listen address")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return serve(*listen)
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ContinueOnError)
		url := fs.String("url", "", "HTTP upload URL")
		size := fs.Int("bytes", 1<<20, "payload size")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return upload(*url, *size)
	case "reject":
		fs := flag.NewFlagSet("reject", flag.ContinueOnError)
		addr := fs.String("addr", "", "edge proxy tunnel address")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return expectTunnelRejection(*addr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           uploadHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return server.ListenAndServe()
}

func uploadHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		h := sha256.New()
		n, err := io.Copy(h, http.MaxBytesReader(w, r.Body, maxUploadBytes))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "%d %x\n", n, h.Sum(nil))
	})
	return mux
}

func upload(url string, size int) error {
	if url == "" || size < 1 {
		return fmt.Errorf("upload needs a URL and positive byte count")
	}
	hash := sha256.New()
	payload := io.TeeReader(newTestPayloadReader(size), hash)
	req, err := http.NewRequest(http.MethodPost, url, payload)
	if err != nil {
		return fmt.Errorf("creating POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(size)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("POST %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	var gotSize int
	var gotHash string
	if _, err := fmt.Fscan(resp.Body, &gotSize, &gotHash); err != nil {
		return fmt.Errorf("reading upload response: %w", err)
	}
	wantHash := fmt.Sprintf("%x", hash.Sum(nil))
	if gotSize != size || gotHash != wantHash {
		return fmt.Errorf("upload mismatch: got %d %s, want %d %s", gotSize, gotHash, size, wantHash)
	}
	fmt.Printf("upload ok: %d bytes sha256=%s\n", gotSize, gotHash)
	return nil
}

type testPayloadReader struct {
	offset int
	size   int
}

func newTestPayloadReader(size int) io.Reader {
	return &testPayloadReader{size: size}
}

func (r *testPayloadReader) Read(p []byte) (int, error) {
	remaining := r.size - r.offset
	if remaining <= 0 {
		return 0, io.EOF
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	for i := range p {
		p[i] = byte(((r.offset+i)*31 + 17) % 251)
	}
	r.offset += len(p)
	return len(p), nil
}

func expectTunnelRejection(addr string) error {
	if addr == "" {
		return fmt.Errorf("reject needs an address")
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dialing tunnel %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s invalid-token 127.0.0.1:1\n", tunnelProtocol); err != nil {
		return err
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if n != 0 {
		return fmt.Errorf("unauthorized tunnel returned %s byte(s)", strconv.Itoa(n))
	}
	if err == nil {
		return fmt.Errorf("unauthorized tunnel stayed open")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return fmt.Errorf("unauthorized tunnel was not closed before timeout")
	}
	fmt.Println("unauthorized tunnel rejected")
	return nil
}
