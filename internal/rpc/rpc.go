// Package rpc holds the JSON-RPC 2.0 envelope both out-of-process transports
// use: the MCP client and the plugin client. Only the wire types and the two
// error helpers live here, because the two protocols differ in their method
// names, capabilities and transport. Sharing the envelope keeps a fix to the
// framing in one place instead of two.
package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CodeMethodNotFound is the JSON-RPC error code for an unknown method. A client
// treats it as "this peer does not implement the call" and falls back instead of
// failing, so both protocols need the same constant.
const CodeMethodNotFound = -32601

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("rpc error %d", e.Code)
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Message is one JSON-RPC frame. A request fills ID, Method and Params; a
// response fills ID plus either Result or Error.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Response is a reply handed to a waiting caller. Exactly one of Result and Err
// is set.
type Response struct {
	Result json.RawMessage
	Err    error
}

// ParseID accepts a numeric id or a quoted numeric id, because peers differ on
// whether they stringify it.
func ParseID(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	return strconv.ParseInt(text, 10, 64)
}

// IsMethodNotFound reports whether a peer refused a call because it does not
// implement that method.
func IsMethodNotFound(err error) bool {
	var rpcErr *Error
	return errors.As(err, &rpcErr) && rpcErr.Code == CodeMethodNotFound
}
