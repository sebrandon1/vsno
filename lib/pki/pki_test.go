package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

func TestIssueAndCovers(t *testing.T) {
	caCert, caKey, err := NewCA("vsno-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	ips := []net.IP{net.ParseIP("172.30.1.2")}
	names := []string{"registry.vsno.svc", "registry.vsno.svc.cluster.local"}
	cert, key, err := IssueServer(caCert, caKey, ips, names, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(key), "EC PRIVATE KEY") {
		t.Fatalf("unexpected key PEM: %s", key)
	}

	b, _ := pem.Decode(cert)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if c.IsCA || c.Subject.CommonName != names[0] || c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("server cert fields wrong: CA=%v CN=%q", c.IsCA, c.Subject.CommonName)
	}

	if !Covers(cert, caCert, ips, names, now) {
		t.Error("cert should cover its own SANs")
	}
	if Covers(cert, caCert, []net.IP{net.ParseIP("172.30.9.9")}, nil, now) {
		t.Error("a new ClusterIP must force a reissue")
	}
	if Covers(cert, caCert, ips, []string{"other.svc"}, now) {
		t.Error("a missing DNS name must force a reissue")
	}
	if Covers(cert, caCert, ips, names, now.Add(11*365*24*time.Hour)) {
		t.Error("an expired cert must force a reissue")
	}
	otherCA, _, _ := NewCA("other", now)
	if Covers(cert, otherCA, ips, names, now) {
		t.Error("a cert from another CA must force a reissue")
	}
	if Covers([]byte("junk"), caCert, ips, names, now) || Covers(cert, []byte("junk"), ips, names, now) {
		t.Error("junk PEM must not be accepted")
	}
}

func TestIssueServerBadCA(t *testing.T) {
	caCert, caKey, _ := NewCA("vsno-ca", now)
	if _, _, err := IssueServer([]byte("x"), caKey, nil, nil, now); err == nil {
		t.Error("want error for bad CA cert")
	}
	if _, _, err := IssueServer(caCert, []byte("x"), nil, nil, now); err == nil {
		t.Error("want error for bad CA key")
	}
	if _, _, err := IssueServer(caCert, []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"), nil, nil, now); err == nil {
		t.Error("want error for corrupt CA key")
	}
	cert, _, err := IssueServer(caCert, caKey, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(cert)
	c, _ := x509.ParseCertificate(b.Bytes)
	if c.Subject.CommonName != "registry" {
		t.Errorf("default CN = %q", c.Subject.CommonName)
	}
}

func TestHtpasswd(t *testing.T) {
	line, err := Htpasswd("vsno", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "vsno:$2") {
		t.Fatalf("not a bcrypt htpasswd line: %q", line)
	}
	if !CheckHtpasswd(line, "vsno", "secret") {
		t.Error("matching password rejected")
	}
	if CheckHtpasswd(line, "vsno", "wrong") || CheckHtpasswd(line, "other", "secret") || CheckHtpasswd("", "vsno", "secret") {
		t.Error("mismatch accepted")
	}
}

func TestRandomHex(t *testing.T) {
	a, err := RandomHex(16)
	if err != nil || len(a) != 32 {
		t.Fatalf("RandomHex(16) = %q, %v", a, err)
	}
	if b, _ := RandomHex(16); a == b {
		t.Error("two random values are equal")
	}
}
