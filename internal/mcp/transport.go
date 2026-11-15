package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"os"
	"regexp"
	"strings"
)

// envReference matches the {env:NAME} placeholder used in URLs
// and headers. ${NAME} and $NAME are handled by os.ExpandEnv.
var envReference = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(value string) string {
	value = envReference.ReplaceAllStringFunc(value, func(match string) string {
		return os.Getenv(envReference.FindStringSubmatch(match)[1])
	})
	return os.ExpandEnv(value)
}

func expandHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[expandEnv(key)] = expandEnv(value)
	}
	return out
}

// decodeMessages accepts one JSON-RPC object or a batch array.
func decodeMessages(data []byte) ([]rpc.Message, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] == '[' {
		var list []rpc.Message
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var message rpc.Message
	if err := json.Unmarshal(data, &message); err != nil {
		return nil, err
	}
	return []rpc.Message{message}, nil
}

// readEventStream parses server-sent events and stops once handle reports that
// the awaited response has been seen.
func readEventStream(ctx context.Context, body io.Reader, handle func(rpc.Message) bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	var data strings.Builder

	flush := func() (bool, error) {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return false, nil
		}
		messages, err := decodeMessages([]byte(payload))
		if err != nil {
			return false, fmt.Errorf("mcp: invalid event data: %w", err)
		}
		for _, message := range messages {
			if handle(message) {
				return true, nil
			}
		}
		return false, nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			stop, err := flush()
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		data.WriteString(strings.TrimPrefix(value, " "))
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("mcp: event stream: %w", err)
	}
	_, err := flush()
	return err
}

func messageIDEquals(raw json.RawMessage, id int64) bool {
	got, err := rpc.ParseID(raw)
	return err == nil && got == id
}
