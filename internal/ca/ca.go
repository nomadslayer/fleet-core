// Package ca owns all identity material for the control plane:
//   - a root CA (ECDSA P-256) that signs agent client certs and the
//     control-plane server cert,
//   - an ed25519 keypair that signs module payloads.
//
// Identity encoding: agent client certs carry CN=<machine-id> and
// OU=<tenant-id>. Every request is attributed from the presented cert,
// never from the request body — tenancy is therefore enforced by mTLS.
package ca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte

	ModulePriv ed25519.PrivateKey
	ModulePub  ed25519.PublicKey
}

// LoadOrCreate initialises CA material under dir on first boot and
// reloads it afterwards.
func LoadOrCreate(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")
	modPath := filepath.Join(dir, "module-signing.key")

	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := create(certPath, keyPath, modPath); err != nil {
			return nil, fmt.Errorf("create ca: %w", err)
		}
	}
	return load(certPath, keyPath, modPath)
}

func create(certPath, keyPath, modPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fleetcore-root-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	_, modPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return err
	}
	modDER, err := x509.MarshalPKCS8PrivateKey(modPriv)
	if err != nil {
		return err
	}
	return writePEM(modPath, "PRIVATE KEY", modDER, 0o600)
}

func load(certPath, keyPath, modPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	keyDER, err := readPEM(keyPath)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	modDER, err := readPEM(modPath)
	if err != nil {
		return nil, err
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(modDER)
	if err != nil {
		return nil, err
	}
	modPriv, ok := anyKey.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("module signing key is not ed25519")
	}
	return &CA{
		Cert:       cert,
		Key:        key,
		CertPEM:    certPEM,
		ModulePriv: modPriv,
		ModulePub:  modPriv.Public().(ed25519.PublicKey),
	}, nil
}

// SignAgent issues an mTLS client certificate for a machine. Only the
// public key is taken from the CSR; the subject is set server-side.
func (c *CA) SignAgent(csrPEM []byte, machineID, tenantID string) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         machineID,
			OrganizationalUnit: []string{tenantID},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// ServerTLSCert issues (in memory) the control-plane's own server cert.
func (c *CA) ServerTLSCert(sans []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "fleetcore-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Present the CA in the chain so bootstrapping agents can verify the
	// server by CA fingerprint before they have anything pinned.
	return tls.Certificate{Certificate: [][]byte{der, c.Cert.Raw}, PrivateKey: key}, nil
}

// SignModule signs a module payload with the ed25519 module key.
func (c *CA) SignModule(payload []byte) []byte { return ed25519.Sign(c.ModulePriv, payload) }

// ModulePubPEM returns the module-verification public key in PEM form.
func (c *CA) ModulePubPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(c.ModulePub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ---- helpers ----

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func readPEM(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return block.Bytes, nil
}

func parseCertPEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
