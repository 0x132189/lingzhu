package mcp

import "strings"

// AuthRequiredError reports a remote server that refused an unauthenticated
// request. ResourceMetadataURL is the protected-resource metadata advertised by
// the server's WWW-Authenticate challenge, when present.
type AuthRequiredError struct {
	Server              string
	ResourceMetadataURL string
}

func (e *AuthRequiredError) Error() string {
	if e.ResourceMetadataURL != "" {
		return "MCP server " + e.Server + " requires authorization (" + e.ResourceMetadataURL + ")"
	}
	return "MCP server " + e.Server + " requires authorization"
}

func authRequired(server, challenge string) *AuthRequiredError {
	err := &AuthRequiredError{Server: server}
	lower := strings.ToLower(challenge)
	for _, prefix := range []string{"resource_metadata=", "resource_metadata ="} {
		index := strings.Index(lower, prefix)
		if index < 0 {
			continue
		}
		value := strings.TrimSpace(challenge[index+len(prefix):])
		if value == "" {
			return err
		}
		if value[0] == '"' {
			if end := strings.IndexByte(value[1:], '"'); end >= 0 {
				err.ResourceMetadataURL = value[1 : 1+end]
			}
			return err
		}
		if end := strings.IndexAny(value, ", \t"); end >= 0 {
			value = value[:end]
		}
		err.ResourceMetadataURL = value
		return err
	}
	return err
}
