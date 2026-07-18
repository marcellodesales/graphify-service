package giturl

import "testing"

func TestParseCanonical(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		canonical string
		host      string
		owner     string
		repo      string
		transport Transport
	}{
		{"https no suffix", "https://github.com/marcellodesales/graphify-service",
			"https://github.com/marcellodesales/graphify-service.git", "github.com", "marcellodesales", "graphify-service", TransportHTTPS},
		{"https with .git", "https://github.com/marcellodesales/graphify-service.git",
			"https://github.com/marcellodesales/graphify-service.git", "github.com", "marcellodesales", "graphify-service", TransportHTTPS},
		{"https trailing slash + upper host", "https://GitHub.com/Owner/Repo/",
			"https://github.com/Owner/Repo.git", "github.com", "Owner", "Repo", TransportHTTPS},
		{"https default port dropped", "https://github.com:443/o/r.git",
			"https://github.com/o/r.git", "github.com", "o", "r", TransportHTTPS},
		{"scp-like", "git@github.com:marcellodesales/graphify-service.git",
			"ssh://git@github.com/marcellodesales/graphify-service.git", "github.com", "marcellodesales", "graphify-service", TransportSSH},
		{"ssh url", "ssh://git@github.com/o/r.git",
			"ssh://git@github.com/o/r.git", "github.com", "o", "r", TransportSSH},
		{"ssh default port dropped", "ssh://git@github.com:22/o/r",
			"ssh://git@github.com/o/r.git", "github.com", "o", "r", TransportSSH},
		{"gitlab subgroup", "https://gitlab.com/group/subgroup/repo.git",
			"https://gitlab.com/group/subgroup/repo.git", "gitlab.com", "group/subgroup", "repo", TransportHTTPS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", c.in, err)
			}
			if got.Canonical != c.canonical {
				t.Errorf("canonical = %q, want %q", got.Canonical, c.canonical)
			}
			if got.Host != c.host || got.Owner != c.owner || got.Name != c.repo {
				t.Errorf("host/owner/repo = %q/%q/%q, want %q/%q/%q", got.Host, got.Owner, got.Name, c.host, c.owner, c.repo)
			}
			if got.Transport != c.transport {
				t.Errorf("transport = %q, want %q", got.Transport, c.transport)
			}
		})
	}
}

func TestParseCanonicalStableAcrossForms(t *testing.T) {
	a, _ := Parse("https://github.com/o/r")
	b, _ := Parse("https://github.com/o/r.git")
	c, _ := Parse("https://GitHub.com/o/r/")
	if a.Canonical != b.Canonical || a.Canonical != c.Canonical {
		t.Fatalf("canonical forms diverge: %q %q %q", a.Canonical, b.Canonical, c.Canonical)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"https://user:token@github.com/o/r.git", // userinfo/token
		"https://github.com/o/r?foo=bar",        // query
		"https://github.com/o/r#frag",           // fragment
		"file:///etc/passwd",                    // file scheme
		"git://github.com/o/r.git",              // unsupported scheme
		"http://github.com/o/r.git",             // plain http not allowed
		"https://github.com/onlyowner",          // missing repo
		"ssh://user:pass@github.com/o/r.git",    // ssh password
		"/local/path/repo",                      // local path
		"https://github.com/o/../r.git",         // traversal segment
	}
	for _, in := range bad {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, got)
		}
	}
}

func TestValidateRef(t *testing.T) {
	ok := []string{"main", "refs/heads/main", "v1.2.3", "release/2026-07", "feature/x_y"}
	for _, r := range ok {
		if err := ValidateRef(r); err != nil {
			t.Errorf("ValidateRef(%q) unexpected error: %v", r, err)
		}
	}
	bad := []string{"", "-x", "a..b", "a b", "he@{ad", "a\\b", "a//b", "with~tilde", "ends/", "x.lock", "ctrl\x01"}
	for _, r := range bad {
		if err := ValidateRef(r); err == nil {
			t.Errorf("ValidateRef(%q) = nil, want error", r)
		}
	}
}
