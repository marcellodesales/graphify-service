package memory

import (
	"fmt"
	"os"
	"strings"
)

// maxSSHKeyBytes caps a caller-supplied private key. Generous for any real key
// format (RSA/EC/ed25519/OpenSSH) while rejecting an accidental large paste.
const maxSSHKeyBytes = 64 << 10 // 64 KiB

// LooksLikePrivateKey reports whether b is plausibly a PEM/OpenSSH *private*
// key. This is a shape check, not cryptographic validation: it guards against
// obviously-wrong input (a public key, an empty body, a giant blob) without
// importing crypto or rejecting any valid private-key encoding. The key is used
// transiently to unlock the clone — the way a password unlocks a protected zip
// — and is never persisted in the manifest.
//
// Passphrase-protected keys are accepted at this layer but will simply fail the
// clone (git runs with BatchMode=yes and no agent, so it never prompts); the
// passphrase itself is a secret we intentionally never accept or store.
func LooksLikePrivateKey(b []byte) bool {
	if len(b) == 0 || len(b) > maxSSHKeyBytes {
		return false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return false
	}
	// Reject public keys explicitly (the common copy/paste mistake).
	if strings.HasPrefix(s, "ssh-") || strings.HasPrefix(s, "ecdsa-") ||
		strings.HasPrefix(s, "sk-ssh-") || strings.HasPrefix(s, "sk-ecdsa-") {
		return false
	}
	return strings.Contains(s, "-----BEGIN ") && strings.Contains(s, "PRIVATE KEY-----")
}

// WriteResourceSSHKey stores a caller-supplied deploy key for a git resource on
// the shared data volume, under the memory's gitignored .ssh/ directory (mode
// 0600). The key material is NEVER written to memory.json; only a non-secret
// boolean flag (Source.SSHKeyStored) records that a stored key exists. Optional
// knownHosts (public host keys — not a secret) is stored alongside for host-key
// verification.
//
// Both id and rid MUST be validated (ValidID / ValidResourceID) by the caller:
// they are used to build the on-disk path.
func WriteResourceSSHKey(l Layout, id, rid string, keyPEM, knownHosts []byte) error {
	if !LooksLikePrivateKey(keyPEM) {
		return fmt.Errorf("memory: sshKey does not look like a PEM private key")
	}
	if err := os.MkdirAll(l.SSHDir(id), 0o700); err != nil {
		return fmt.Errorf("memory: ssh dir: %w", err)
	}
	// ssh requires a trailing newline on key files; normalize CRLF/whitespace.
	norm := []byte(strings.TrimRight(string(keyPEM), "\r\n ") + "\n")
	if err := writeFile0600(l.SSHKeyPath(id, rid), norm); err != nil {
		return err
	}
	if len(strings.TrimSpace(string(knownHosts))) > 0 {
		kh := []byte(strings.TrimRight(string(knownHosts), "\r\n ") + "\n")
		if err := writeFile0600(l.SSHKnownHostsPath(id, rid), kh); err != nil {
			return err
		}
	}
	return nil
}

// HasResourceSSHKey reports whether a stored caller-supplied deploy key exists
// for the resource. This on-disk presence is the source of truth the worker
// keys on (the key "travels with the task").
func HasResourceSSHKey(l Layout, id, rid string) bool {
	fi, err := os.Stat(l.SSHKeyPath(id, rid))
	return err == nil && fi.Mode().IsRegular()
}

// HasResourceKnownHosts reports whether stored known_hosts exist for the resource.
func HasResourceKnownHosts(l Layout, id, rid string) bool {
	fi, err := os.Stat(l.SSHKnownHostsPath(id, rid))
	return err == nil && fi.Mode().IsRegular()
}

// writeFile0600 writes b to path with mode 0600 via a temp file + rename so a
// reader never sees a partial key, and chmods explicitly (umask cannot add
// bits, but a pre-existing temp would keep its mode otherwise).
func writeFile0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("memory: write ssh file: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("memory: chmod ssh file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("memory: publish ssh file: %w", err)
	}
	return nil
}
