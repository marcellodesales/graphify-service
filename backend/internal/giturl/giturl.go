// Package giturl parses and canonicalizes Git repository URLs.
//
// It accepts HTTPS, ssh://, and scp-like (git@host:owner/repo) forms and
// produces a single canonical URL used both for storage identity and for the
// deterministic repository ID. Credentials, query strings, fragments, and
// non-git transports are rejected (see spec §5.2, §22.5).
package giturl

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Transport is the wire protocol family of a repository URL.
type Transport string

const (
	// TransportHTTPS is an https:// repository URL.
	TransportHTTPS Transport = "https"
	// TransportSSH is an ssh:// or scp-like repository URL.
	TransportSSH Transport = "ssh"
)

// Repo is a parsed, canonicalized repository reference.
type Repo struct {
	Canonical string    // canonical URL, e.g. https://github.com/owner/repo.git
	Host      string    // lowercased host, e.g. github.com
	Owner     string    // owner path (may contain subgroups), e.g. marcellodesales
	Name      string    // repository name without .git
	Transport Transport // https or ssh
	User      string    // ssh user (empty for https)
}

// Sentinel errors returned by Parse.
var (
	ErrEmpty         = errors.New("giturl: empty url")
	ErrUnsupported   = errors.New("giturl: unsupported url or scheme")
	ErrCredentials   = errors.New("giturl: url must not contain userinfo credentials")
	ErrQueryFragment = errors.New("giturl: url must not contain a query string or fragment")
	ErrPath          = errors.New("giturl: url must contain <owner>/<repo>")
)

// Parse parses and canonicalizes a Git repository URL.
func Parse(raw string) (Repo, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Repo{}, ErrEmpty
	}

	// scp-like form: [user@]host:owner/repo(.git) — no scheme, no "//".
	if !strings.Contains(s, "://") {
		return parseSCP(s)
	}

	u, err := url.Parse(s)
	if err != nil {
		return Repo{}, fmt.Errorf("giturl: %v", err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Repo{}, ErrQueryFragment
	}

	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.User != nil {
			return Repo{}, ErrCredentials
		}
		return build(TransportHTTPS, "", u.Hostname(), u.Port(), u.Path)
	case "ssh":
		user := "git"
		if u.User != nil {
			if _, hasPass := u.User.Password(); hasPass {
				return Repo{}, ErrCredentials
			}
			if name := u.User.Username(); name != "" {
				user = name
			}
		}
		return build(TransportSSH, user, u.Hostname(), u.Port(), u.Path)
	default:
		return Repo{}, fmt.Errorf("%w: scheme %q", ErrUnsupported, u.Scheme)
	}
}

func parseSCP(s string) (Repo, error) {
	user := "git"
	rest := s
	if at := strings.Index(s, "@"); at >= 0 {
		user = s[:at]
		rest = s[at+1:]
	}
	if user == "" {
		user = "git"
	}
	if strings.ContainsAny(user, ":/") {
		return Repo{}, ErrCredentials
	}
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return Repo{}, fmt.Errorf("%w: %q", ErrUnsupported, s)
	}
	host := rest[:colon]
	path := rest[colon+1:]
	// A scp-like path is relative (owner/repo), never absolute or a port number.
	return build(TransportSSH, user, host, "", path)
}

func build(t Transport, user, host, port, path string) (Repo, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return Repo{}, fmt.Errorf("%w: missing host", ErrUnsupported)
	}
	if (t == TransportHTTPS && port == "443") || (t == TransportSSH && port == "22") {
		port = ""
	}
	owner, name, err := splitOwnerRepo(path)
	if err != nil {
		return Repo{}, err
	}
	hostport := host
	if port != "" {
		hostport = host + ":" + port
	}

	var canonical string
	switch t {
	case TransportHTTPS:
		canonical = "https://" + hostport + "/" + owner + "/" + name + ".git"
	case TransportSSH:
		if user == "" {
			user = "git"
		}
		canonical = "ssh://" + user + "@" + hostport + "/" + owner + "/" + name + ".git"
	}
	return Repo{
		Canonical: canonical,
		Host:      host,
		Owner:     owner,
		Name:      name,
		Transport: t,
		User:      user,
	}, nil
}

func splitOwnerRepo(p string) (owner, name string, err error) {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" {
		return "", "", ErrPath
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", "", ErrPath
	}
	for _, seg := range segs {
		if err := validateSegment(seg); err != nil {
			return "", "", err
		}
	}
	name = segs[len(segs)-1]
	owner = strings.Join(segs[:len(segs)-1], "/")
	return owner, name, nil
}

func validateSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." {
		return fmt.Errorf("%w: invalid path segment %q", ErrPath, seg)
	}
	for _, r := range seg {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("%w: control character in path", ErrPath)
		}
	}
	return nil
}

// ValidateRef checks that ref is a syntactically valid Git ref/branch/tag name
// safe to hand to git. It rejects the sequences and characters git itself
// forbids (see git-check-ref-format) plus a leading "-".
func ValidateRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("giturl: empty ref")
	}
	if strings.HasPrefix(ref, "-") {
		return errors.New("giturl: ref must not start with '-'")
	}
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return errors.New("giturl: invalid ref suffix")
	}
	for _, bad := range []string{"..", "@{", "\\", "//"} {
		if strings.Contains(ref, bad) {
			return fmt.Errorf("giturl: ref contains forbidden sequence %q", bad)
		}
	}
	for _, r := range ref {
		switch {
		case r < 0x20, r == 0x7f:
			return errors.New("giturl: ref contains a control character")
		case r == ' ', r == '~', r == '^', r == ':', r == '?', r == '*', r == '[':
			return fmt.Errorf("giturl: ref contains forbidden character %q", string(r))
		}
	}
	return nil
}
