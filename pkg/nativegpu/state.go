package nativegpu

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServiceState owns a private state-directory lock and a specific local TCP
// interface. Public clients receive the CA, never the CA key or admin token.
type ServiceState struct {
	Dir         string
	Endpoint    string
	AdminToken  string
	listener    net.Listener
	certificate tls.Certificate
	unlock      func()
}

func OpenServiceState(dir, address string) (_ *ServiceState, err error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("native GPU state directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return nil, errors.New("native GPU state directory must be a real, private directory (mode 0700)")
	}
	unlock, err := lockState(filepath.Join(dir, "service.lock"))
	if err != nil {
		return nil, err
	}
	state := &ServiceState{Dir: dir, unlock: unlock}
	defer func() {
		if err != nil {
			state.Close()
		}
	}()
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("native GPU listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return nil, errors.New("native GPU listener requires an explicit loopback or private interface IP; wildcard/public listeners are not supported")
	}
	if !ip.IsLoopback() {
		addresses, listErr := net.InterfaceAddrs()
		if listErr != nil {
			return nil, listErr
		}
		found := false
		for _, address := range addresses {
			candidate, _, parseErr := net.ParseCIDR(address.String())
			if parseErr == nil && ip.Equal(candidate) {
				found = true
			}
		}
		if !found {
			return nil, errors.New("native GPU listen IP is not assigned to this Mac")
		}
	}
	state.AdminToken, err = loadOrCreateToken(filepath.Join(dir, "admin.token"))
	if err != nil {
		return nil, err
	}
	state.certificate, err = serviceCertificate(dir, ip)
	if err != nil {
		return nil, err
	}
	state.listener, err = net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	state.Endpoint = "https://" + state.listener.Addr().String()
	return state, nil
}

func (s *ServiceState) Close() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
}

func (s *ServiceState) Serve(ctx context.Context, broker *Broker) error {
	defer broker.Close()
	info, _ := json.Marshal(map[string]any{"version": 1, "endpoint": s.Endpoint})
	if err := writeAtomic(filepath.Join(s.Dir, "service.json"), info, 0o600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(s.Dir, "service.json"))
	server := &http.Server{Handler: broker, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	listener := tls.NewListener(s.listener, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.certificate}})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		broker.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		<-done
		return nil
	}
}

func loadOrCreateToken(path string) (string, error) {
	data, err := readPrivate(path, 4096)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) != 64 {
			return "", errors.New("invalid native GPU admin credential; use a new private state directory")
		}
		return token, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	token, err := secret()
	if err != nil {
		return "", err
	}
	return token, writeAtomic(path, []byte(token), 0o600)
}

func readPrivate(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maximum || !ownedByCurrentUser(info) {
		return nil, errors.New("native GPU private file must be regular, owner-only and within its size limit")
	}
	return os.ReadFile(path)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".native-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }

func serviceCertificate(dir string, ip net.IP) (tls.Certificate, error) {
	caPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	var ca *x509.Certificate
	var key *ecdsa.PrivateKey
	keyData, err := readPrivate(keyPath, 16384)
	if os.IsNotExist(err) {
		if _, statErr := os.Lstat(caPath); !os.IsNotExist(statErr) {
			return tls.Certificate{}, errors.New("incomplete native GPU CA; use a new private state directory")
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
		number, err := serial()
		if err != nil {
			return tls.Certificate{}, err
		}
		ca = &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: "Kiac native GPU local CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
		if err != nil {
			return tls.Certificate{}, err
		}
		encoded, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
			return tls.Certificate{}, err
		}
		if err := writeAtomic(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			return tls.Certificate{}, err
		}
		ca, err = x509.ParseCertificate(der)
		if err != nil {
			return tls.Certificate{}, err
		}
	} else if err != nil {
		return tls.Certificate{}, err
	} else {
		block, _ := pem.Decode(keyData)
		if block == nil {
			return tls.Certificate{}, errors.New("invalid native GPU CA private key")
		}
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return tls.Certificate{}, err
		}
		data, err := os.ReadFile(caPath)
		if err != nil {
			return tls.Certificate{}, err
		}
		block, _ = pem.Decode(data)
		if block == nil {
			return tls.Certificate{}, errors.New("invalid native GPU CA certificate")
		}
		ca, err = x509.ParseCertificate(block.Bytes)
		if err != nil {
			return tls.Certificate{}, err
		}
		public, ok := ca.PublicKey.(*ecdsa.PublicKey)
		if !ca.IsCA || !ok || !public.Equal(&key.PublicKey) || time.Now().After(ca.NotAfter) {
			return tls.Certificate{}, errors.New("native GPU CA key/certificate mismatch or expired CA")
		}
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	number, err := serial()
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: "Kiac native GPU"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: ca.NotAfter, IPAddresses: []net.IP{ip}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	encoded, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
}
