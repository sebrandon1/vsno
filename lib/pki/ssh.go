package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHKey returns a new ed25519 key pair: the private key in OpenSSH PEM form
// and the public key as an authorized_keys line with comment.
func SSHKey(comment string) (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	return string(pem.EncodeToMemory(block)), authorized, nil
}

// SSHKeyMatches reports whether public is private's public key, so a stored
// pair can be reused.
func SSHKeyMatches(private, public string) bool {
	signer, err := ssh.ParsePrivateKey([]byte(private))
	if err != nil {
		return false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(public))
	return err == nil && string(pub.Marshal()) == string(signer.PublicKey().Marshal())
}
