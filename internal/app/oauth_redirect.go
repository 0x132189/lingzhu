package app

import (
	"net"
	"strings"
)

// oauthRedirect is the loopback callback the core serves for MCP OAuth. A
// wildcard or empty listen address is replaced with 127.0.0.1 because the
// browser must be able to reach it.
func oauthRedirect(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8787/oauth/callback"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/oauth/callback"
}
