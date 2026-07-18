// Package config loads the graphify-service configuration from the environment.
//
// Every knob has a documented default (see docs/FEATURES-BACKEND-SERVICE.md §17)
// so the service is runnable out of the box for local development.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// AuthMode selects how bearer tokens on protected routes are validated.
type AuthMode string

const (
	// AuthNone disables authentication (local development only).
	AuthNone AuthMode = "none"
	// AuthStatic validates a single shared bearer token (GRAPHIFY_API_TOKEN).
	AuthStatic AuthMode = "static"
	// AuthOIDC validates external OIDC JWTs. Not implemented in Phase 1.
	AuthOIDC AuthMode = "oidc"
)

// Config is the fully-resolved service configuration.
type Config struct {
	Env             string        // GRAPHIFY_ENV (development|production)
	HTTPAddr        string        // GRAPHIFY_HTTP_ADDR
	ReposRoot       string        // GRAPHIFY_REPOS_ROOT
	AuthMode        AuthMode      // GRAPHIFY_AUTH_MODE
	APIToken        string        // GRAPHIFY_API_TOKEN
	MaxRequestBytes int64         // GRAPHIFY_MAX_REQUEST_BYTES
	LogLevel        string        // GRAPHIFY_LOG_LEVEL
	AllowedGitHosts []string      // GRAPHIFY_ALLOWED_GIT_HOSTS (comma-separated)
	CloneTimeout    time.Duration // GRAPHIFY_CLONE_TIMEOUT
	RunTimeout      time.Duration // GRAPHIFY_RUN_TIMEOUT
	MCPStateless    bool          // GRAPHIFY_MCP_STATELESS
}

// Load reads configuration from the environment, applies defaults, and validates.
func Load() (Config, error) {
	c := Config{
		Env:             getenv("GRAPHIFY_ENV", "development"),
		HTTPAddr:        getenv("GRAPHIFY_HTTP_ADDR", ":8080"),
		ReposRoot:       getenv("GRAPHIFY_REPOS_ROOT", "/graphify-service/repos"),
		AuthMode:        AuthMode(strings.ToLower(getenv("GRAPHIFY_AUTH_MODE", "none"))),
		APIToken:        os.Getenv("GRAPHIFY_API_TOKEN"),
		LogLevel:        getenv("GRAPHIFY_LOG_LEVEL", "info"),
		AllowedGitHosts: splitCSV(os.Getenv("GRAPHIFY_ALLOWED_GIT_HOSTS")),
		MCPStateless:    getbool("GRAPHIFY_MCP_STATELESS", true),
	}

	var err error
	if c.MaxRequestBytes, err = parseSize(getenv("GRAPHIFY_MAX_REQUEST_BYTES", "1MiB")); err != nil {
		return Config{}, fmt.Errorf("GRAPHIFY_MAX_REQUEST_BYTES: %w", err)
	}
	if c.CloneTimeout, err = time.ParseDuration(getenv("GRAPHIFY_CLONE_TIMEOUT", "10m")); err != nil {
		return Config{}, fmt.Errorf("GRAPHIFY_CLONE_TIMEOUT: %w", err)
	}
	if c.RunTimeout, err = time.ParseDuration(getenv("GRAPHIFY_RUN_TIMEOUT", "60m")); err != nil {
		return Config{}, fmt.Errorf("GRAPHIFY_RUN_TIMEOUT: %w", err)
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	switch c.AuthMode {
	case AuthNone, AuthStatic, AuthOIDC:
	default:
		return fmt.Errorf("invalid GRAPHIFY_AUTH_MODE %q (want none|static|oidc)", c.AuthMode)
	}
	if c.AuthMode == AuthStatic && c.APIToken == "" {
		return fmt.Errorf("GRAPHIFY_AUTH_MODE=static requires GRAPHIFY_API_TOKEN")
	}
	if c.AuthMode == AuthOIDC {
		return fmt.Errorf("GRAPHIFY_AUTH_MODE=oidc is not implemented in Phase 1")
	}
	// Production must not run unauthenticated on a non-loopback bind unless
	// explicitly overridden. See spec §17.
	if c.Env == "production" && c.AuthMode == AuthNone && !isLoopbackAddr(c.HTTPAddr) {
		if os.Getenv("GRAPHIFY_INSECURE_ALLOW_NO_AUTH") != "1" {
			return fmt.Errorf("refusing to start: production bind %q is non-loopback with GRAPHIFY_AUTH_MODE=none (set GRAPHIFY_INSECURE_ALLOW_NO_AUTH=1 to override)", c.HTTPAddr)
		}
	}
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("GRAPHIFY_MAX_REQUEST_BYTES must be positive")
	}
	return nil
}

// isLoopbackAddr reports whether addr (host:port) binds only to loopback.
// An empty or wildcard host (":8080", "0.0.0.0:8080") is NOT loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getbool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

// parseSize parses a byte size such as "1MiB", "512KiB", "10MB", or a plain
// integer number of bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	unit := strings.TrimSpace(s[i:])
	var mult float64
	switch strings.ToLower(unit) {
	case "", "b":
		mult = 1
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "kb":
		mult = 1e3
	case "mb":
		mult = 1e6
	case "gb":
		mult = 1e9
	default:
		return 0, fmt.Errorf("unknown size unit %q", unit)
	}
	return int64(num * mult), nil
}
