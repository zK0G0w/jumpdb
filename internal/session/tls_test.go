package session

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIntermediateRequiresTrustedRoot(t *testing.T) {
	now := time.Now()
	root, rootKey := issueCertificate(t, nil, nil, true, now.Add(-time.Hour), now.Add(time.Hour))
	intermediate, intermediateKey := issueCertificate(t, root, rootKey, true, now.Add(-time.Hour), now.Add(time.Hour))
	leaf, leafKey := issueCertificate(t, intermediate, intermediateKey, false, now.Add(-time.Hour), now.Add(time.Hour))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: leafKey}}}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(root)
	for _, test := range []struct {
		name       string
		pool       *x509.CertPool
		serverName string
		want       bool
	}{
		{"补链成功", rootsWithIntermediate(roots, intermediate), "localhost", true},
		{"缺少中间证书", roots, "localhost", false},
		{"未知根", rootsWithIntermediate(x509.NewCertPool(), intermediate), "localhost", false},
		{"错误主机名", rootsWithIntermediate(roots, intermediate), "wrong.example", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: test.pool, ServerName: test.serverName, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			response, err := client.Get(server.URL)
			if response != nil {
				response.Body.Close()
			}
			if (err == nil) != test.want {
				t.Fatalf("TLS 校验结果不符: %v", err)
			}
		})
	}
	expired, _ := issueCertificate(t, root, rootKey, true, now.Add(-2*time.Hour), now.Add(-time.Hour))
	pool := rootsWithIntermediate(roots, expired)
	_, err := expired.Verify(x509.VerifyOptions{Roots: pool})
	if err == nil {
		t.Fatal("过期证书被接受")
	}
}

func issueCertificate(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ca bool, notBefore, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "synthetic"}, NotBefore: notBefore, NotAfter: notAfter, BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, DNSNames: []string{"localhost"}}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
		template.MaxPathLen = 1
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}
