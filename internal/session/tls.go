package session

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"net/http"
	"time"
)

//go:embed intermediate.pem
var intermediatePEM []byte

func newHTTPClient() (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, ErrRequest
	}
	block, _ := pem.Decode(intermediatePEM)
	if block == nil {
		return nil, ErrRequest
	}
	intermediate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, ErrRequest
	}
	pool := rootsWithIntermediate(roots, intermediate)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect disabled") }}, nil
}

func rootsWithIntermediate(roots *x509.CertPool, intermediate *x509.Certificate) *x509.CertPool {
	pool := roots.Clone()
	// 补链证书只能在自身能链接系统根时作为构链锚点，避免扩大信任范围。
	pool.AddCertWithConstraint(intermediate, func(chain []*x509.Certificate) error {
		intermediates := x509.NewCertPool()
		intermediates.AddCert(intermediate)
		for _, certificate := range chain[1:] {
			intermediates.AddCert(certificate)
		}
		_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
		return err
	})
	return pool
}
