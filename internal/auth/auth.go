// Package auth protects the core's HTTP API. The core listens on a loopback
// address, but that alone only excludes other machines: any local process and
// any web page the browser loads can reach it. A bearer token issued by the
// core closes both gaps, because a cross-site form post cannot set a header.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TokenEnv overrides the stored token. A desktop client that starts the core
// sets it so both sides agree without reading the data directory.
const TokenEnv = "LINGZHU_TOKEN"

// tokenFile is where a core keeps the token it generated for one data
// directory. The app reads the same file to reach a core it did not start.
const tokenFile = "api-token"

// Header is the request header carrying the token. A bearer Authorization
// header is accepted as well, so ordinary HTTP clients work unchanged.
const Header = "X-Lingzhu-Token"

// Token returns the token for a data directory, creating it on first use. An
// explicit LINGZHU_TOKEN wins so a launcher can pin one value, but the chosen
// token is always stored, so a later process reaches the same core.
func Token(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", errors.New("auth: data directory is required")
	}
	path := filepath.Join(dataDir, tokenFile)
	token := strings.TrimSpace(os.Getenv(TokenEnv))
	if token == "" {
		if data, err := os.ReadFile(path); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	if token == "" {
		generated, err := generate()
		if err != nil {
			return "", err
		}
		token = generated
	}
	if stored, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(stored)) != token {
		if err := store(dataDir, path, token); err != nil {
			return "", err
		}
	}
	return token, nil
}

// store writes the token through a temp file, so a half-written token is never
// observed by a client reading it concurrently.
func store(dataDir, path, token string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// generate returns a 32-byte random token.
func generate() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// PublicPath reports whether a request path is served without a token. Only
// the health probe and a share link qualify: a share is addressed by its own
// unguessable token, and the probe must work before a client has one.
func PublicPath(method, path string) bool {
	if method == http.MethodGet && path == "/healthz" {
		return true
	}
	if method == http.MethodGet && path == "/oauth/callback" {
		return true
	}
	return method == http.MethodGet && strings.HasPrefix(path, "/share/")
}

// Middleware rejects requests that carry neither a matching token nor a
// public path. A browser request marked cross-site is refused even with a
// valid token, so a leaked link cannot drive the API from another origin.
func Middleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if PublicPath(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			deny(w, r, "cross-site request rejected")
			return
		}
		if !matches(r, token) {
			deny(w, r, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// matches compares the presented token in constant time.
func matches(r *http.Request, token string) bool {
	presented := strings.TrimSpace(r.Header.Get(Header))
	if presented == "" {
		if header := r.Header.Get("Authorization"); header != "" {
			if value, ok := strings.CutPrefix(header, "Bearer "); ok {
				presented = strings.TrimSpace(value)
			}
		}
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// deny writes an unauthorized response. A browser navigation gets a basic
// challenge so a manual visit still has a path forward.
func deny(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		w.Header().Set("WWW-Authenticate", `Basic realm="lingzhu"`)
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}` + "\n"))
}
