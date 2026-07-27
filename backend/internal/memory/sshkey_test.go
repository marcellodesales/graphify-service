package memory

import (
	"os"
	"strings"
	"testing"
)

const testPrivKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDtESTKEY0000000000000000000000000000000000000000
-----AAAA
-----END OPENSSH PRIVATE KEY-----`

func TestLooksLikePrivateKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"openssh", testPrivKey, true},
		{"rsa", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", true},
		{"ec", "-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----", true},
		{"leading-space", "  \n-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n", true},
		{"public-ed25519", "ssh-ed25519 AAAAC3Nz user@host", false},
		{"public-ecdsa", "ecdsa-sha2-nistp256 AAAAE2Vj user@host", false},
		{"empty", "", false},
		{"garbage", "not a key at all", false},
		{"oversized", strings.Repeat("A", maxSSHKeyBytes+1), false},
	}
	for _, c := range cases {
		if got := LooksLikePrivateKey([]byte(c.in)); got != c.want {
			t.Errorf("%s: LooksLikePrivateKey=%v want %v", c.name, got, c.want)
		}
	}
}

func TestWriteResourceSSHKey(t *testing.T) {
	l := NewLayout(t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	const rid = "fedcba9876543210fedcba9876543210"
	if err := os.MkdirAll(l.MemoryDir(id), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := WriteResourceSSHKey(l, id, rid, []byte(testPrivKey), []byte("github.com ssh-ed25519 AAAA")); err != nil {
		t.Fatalf("WriteResourceSSHKey: %v", err)
	}

	// Key present, reported, and mode 0600.
	if !HasResourceSSHKey(l, id, rid) {
		t.Fatal("HasResourceSSHKey=false after write")
	}
	fi, err := os.Stat(l.SSHKeyPath(id, rid))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode=%v want 0600", fi.Mode().Perm())
	}

	// Stored under the gitignored .ssh/ dir — not under git/, files/, or graphify-out/.
	kp := l.SSHKeyPath(id, rid)
	if !strings.HasPrefix(kp, l.SSHDir(id)+string(os.PathSeparator)) {
		t.Errorf("key path %q not under .ssh dir %q", kp, l.SSHDir(id))
	}
	for _, forbidden := range []string{l.GitDir(id), l.FilesDir(id), l.GraphOutDir(id)} {
		if strings.HasPrefix(kp, forbidden+string(os.PathSeparator)) {
			t.Errorf("key path %q must not live under %q (would be extracted/committed)", kp, forbidden)
		}
	}

	// Trailing newline is enforced (ssh requires it).
	b, err := os.ReadFile(kp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("stored key missing trailing newline")
	}

	// known_hosts stored alongside and reported.
	if !HasResourceKnownHosts(l, id, rid) {
		t.Error("HasResourceKnownHosts=false after write with knownHosts")
	}

	// A public key is rejected and nothing is written for a fresh resource.
	const rid2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := WriteResourceSSHKey(l, id, rid2, []byte("ssh-ed25519 AAAA user@host"), nil); err == nil {
		t.Error("expected error for public key")
	}
	if HasResourceSSHKey(l, id, rid2) {
		t.Error("no key file should exist after a rejected write")
	}
}

func TestScaffoldGitignoresSSHDir(t *testing.T) {
	l := NewLayout(t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	if err := WriteScaffold(l, Memory{ID: id, Name: "t", Status: StatusCreated}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.GitignorePath(id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "/.ssh/") {
		t.Errorf(".gitignore does not ignore /.ssh/:\n%s", b)
	}
}
