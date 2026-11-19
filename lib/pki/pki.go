// Package pki generates the in-cluster registry's CA, server certificate and
// htpasswd credentials.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const validity = 10 * 365 * 24 * time.Hour

// NewCA returns a self-signed CA certificate and its key, both PEM.
func NewCA(cn string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tpl, err := certTemplate(cn, now)
	if err != nil {
		return nil, nil, err
	}
	tpl.IsCA = true
	tpl.BasicConstraintsValid = true
	tpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return encode(der, key)
}

// IssueServer signs a server certificate for the given IPs and DNS names.
func IssueServer(caCertPEM, caKeyPEM []byte, ips []net.IP, dnsNames []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	ca, caKey, err := parsePair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cn := "registry"
	if len(dnsNames) > 0 {
		cn = dnsNames[0]
	}
	tpl, err := certTemplate(cn, now)
	if err != nil {
		return nil, nil, err
	}
	tpl.KeyUsage = x509.KeyUsageDigitalSignature
	tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tpl.IPAddresses = ips
	tpl.DNSNames = dnsNames
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return encode(der, key)
}

// Covers reports whether certPEM was signed by caPEM, is valid at now, and
// includes every IP and DNS name. Setup uses it to decide whether to reissue.
func Covers(certPEM, caPEM []byte, ips []net.IP, dnsNames []string, now time.Time) bool {
	cert, err := parseCert(certPEM)
	if err != nil {
		return false
	}
	ca, err := parseCert(caPEM)
	if err != nil || cert.CheckSignatureFrom(ca) != nil {
		return false
	}
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return false
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(cert.IPAddresses, ip.Equal) {
			return false
		}
	}
	for _, d := range dnsNames {
		if !slices.Contains(cert.DNSNames, d) {
			return false
		}
	}
	return true
}

// Htpasswd returns a bcrypt htpasswd line for user.
func Htpasswd(user, password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return user + ":" + string(h), nil
}

// CheckHtpasswd reports whether line is user's htpasswd entry for password.
func CheckHtpasswd(line, user, password string) bool {
	hash, ok := strings.CutPrefix(line, user+":")
	return ok && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// RandomHex returns n random bytes, hex encoded.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func certTemplate(cn string, now time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"vsno"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
	}, nil
}

func encode(der []byte, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func parsePair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("CA certificate: %w", err)
	}
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, nil, errors.New("CA key: no PEM block")
	}
	key, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("CA key: %w", err)
	}
	return cert, key, nil
}
