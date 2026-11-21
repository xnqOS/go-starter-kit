package logging

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test log CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{"logs.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey}, path
}

func TestTLSChecksCertificateAndServerName(t *testing.T) {
	certificate, caPath := testCertificate(t)
	for _, test := range []struct {
		name, serverName   string
		trust, wantSuccess bool
	}{
		{"trusted", "", true, true},
		{"wrong server name", "wrong.test", true, false},
		{"untrusted CA", "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Syslog.Network, cfg.Syslog.Address = "tls", "logs.test:6514"
			cfg.Syslog.Timeout = 2 * time.Second
			cfg.Syslog.TLSServerName = test.serverName
			if test.trust {
				cfg.Syslog.TLSCAFile = caPath
			}
			client, err := newSyslogClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			local, remote := net.Pipe()
			defer remote.Close()
			client.dial = func(context.Context) (net.Conn, error) { return local, nil }
			serverResult := make(chan error, 1)
			go func() {
				defer remote.Close()
				_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
				server := tls.Server(remote, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if err := server.Handshake(); err != nil {
					serverResult <- err
					return
				}
				message, err := readFrame(bufio.NewReader(server))
				if err == nil && string(message) != "encrypted record" {
					t.Error("TLS payload changed")
				}
				serverResult <- err
			}()
			err = client.Send([]byte("encrypted record"))
			if (err == nil) != test.wantSuccess {
				t.Fatalf("TLS success=%v, want %v: %v", err == nil, test.wantSuccess, err)
			}
			select {
			case serverErr := <-serverResult:
				if test.wantSuccess && serverErr != nil {
					t.Fatal(serverErr)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("TLS peer did not stop")
			}
		})
	}
}

func TestInvalidCAFailsBeforeLogging(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Outputs = []string{"syslog"}
	cfg.Syslog.Network = "tls"
	cfg.Syslog.TLSCAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, _, err := New(cfg); err == nil {
		t.Fatal("accepted missing CA file")
	}
	if err := os.WriteFile(cfg.Syslog.TLSCAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(cfg); err == nil {
		t.Fatal("accepted invalid CA file")
	}
}
