package pki

import (
	"strings"
	"testing"
)

func TestSSHKey(t *testing.T) {
	priv, pub, err := SSHKey("vsno")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(priv, "-----BEGIN OPENSSH PRIVATE KEY-----") || !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " vsno") {
		t.Errorf("unexpected key format:\n%s\n%s", priv, pub)
	}
	if !SSHKeyMatches(priv, pub) {
		t.Error("a fresh pair must match")
	}
	_, other, _ := SSHKey("vsno")
	for _, c := range []struct{ priv, pub string }{{priv, other}, {"garbage", pub}, {priv, "garbage"}} {
		if SSHKeyMatches(c.priv, c.pub) {
			t.Errorf("SSHKeyMatches(%.20q, %.20q) = true", c.priv, c.pub)
		}
	}
}
