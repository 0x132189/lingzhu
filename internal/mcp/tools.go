package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"lingzhu/internal/model"
	"lingzhu/internal/tool"
)

// maxAttachmentBytes bounds one inline MCP media attachment.
const maxAttachmentBytes = 20 * 1024 * 1024

type mcpTool struct {
	client      *Client
	server      string
	rawName     string
	name        string
	description string
	schema      json.RawMessage
}

func (t *mcpTool) Name() string { return t.name }

func (t *mcpTool) Description() string {
	if t.description != "" {
		return t.description
	}
	return fmt.Sprintf("MCP tool %q from server %q.", t.rawName, t.server)
}

func (t *mcpTool) Schema() json.RawMessage {
	if len(t.schema) == 0 || string(t.schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.schema
}

func (t *mcpTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, t.name, []string{"*"}, ""); err != nil {
		return "", err
	}
	args := map[string]any{}
	if len(call.Input) > 0 && string(call.Input) != "null" {
		if err := json.Unmarshal(call.Input, &args); err != nil {
			return "", fmt.Errorf("mcp: %s: invalid arguments: %w", t.name, err)
		}
	}
	result, err := t.client.CallTool(ctx, t.rawName, args)
	if err != nil {
		return "", fmt.Errorf("mcp: %s: %w", t.name, err)
	}
	text := formatCallResult(result)
	if result.IsError {
		if text == "" {
			text = fmt.Sprintf("MCP tool %q returned an error", t.rawName)
		}
		return "", errors.New(text)
	}
	attachResultMedia(call, result)
	return text, nil
}

// attachResultMedia gives images and PDFs returned by a tool to the model as
// inline attachments. Other binary content remains a text placeholder.
func attachResultMedia(call tool.Call, result *CallResult) {
	if result == nil || !call.CanAttach {
		return
	}
	for _, content := range result.Content {
		switch content.Type {
		case "image":
			if content.Data == "" || !attachableMime(content.MimeType) {
				continue
			}
			if base64Size(content.Data) > maxAttachmentBytes {
				continue
			}
			call.SetAttachment(model.Attachment{
				Mime: content.MimeType,
				Name: content.Name,
				Data: content.Data,
			})
		case "resource":
			resource := content.Resource
			if resource == nil || resource.Blob == "" || !attachableMime(resource.MimeType) {
				continue
			}
			if base64Size(resource.Blob) > maxAttachmentBytes {
				continue
			}
			call.SetAttachment(model.Attachment{
				Mime: resource.MimeType,
				Name: content.Name,
				Data: resource.Blob,
			})
		}
	}
}

func attachableMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	return strings.HasPrefix(mime, "image/") || mime == "application/pdf"
}

func attachResourceContents(call tool.Call, contents []ResourceContent) {
	if !call.CanAttach {
		return
	}
	for _, content := range contents {
		if content.Blob == "" || !attachableMime(content.MimeType) || base64Size(content.Blob) > maxAttachmentBytes {
			continue
		}
		call.SetAttachment(model.Attachment{
			Mime: content.MimeType,
			Name: content.URI,
			Data: content.Blob,
		})
	}
}

func formatCallResult(result *CallResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		switch content.Type {
		case "text":
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s %s, %d bytes]", content.Type, content.MimeType, base64Size(content.Data)))
		case "resource":
			if content.Resource != nil {
				parts = append(parts, formatResourceContent(*content.Resource))
			} else if content.Text != "" {
				parts = append(parts, content.Text)
			} else if content.URI != "" {
				parts = append(parts, fmt.Sprintf("[resource %s]", content.URI))
			}
		case "resource_link":
			label := content.URI
			if content.Name != "" {
				label = content.Name + " (" + content.URI + ")"
			}
			if label != "" {
				parts = append(parts, fmt.Sprintf("[resource link %s]", label))
			}
		default:
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		}
	}
	structured := ""
	if len(result.StructuredContent) > 0 {
		var value any
		if err := json.Unmarshal(result.StructuredContent, &value); err == nil {
			if pretty, err := json.MarshalIndent(value, "", "  "); err == nil {
				structured = string(pretty)
			}
		}
	}
	if structured == "" {
		return strings.Join(parts, "\n")
	}
	if len(parts) == 0 {
		return structured
	}
	return strings.Join(parts, "\n") + "\n\n" + structured
}

func base64Size(data string) int {
	if data == "" {
		return 0
	}
	return base64.StdEncoding.DecodedLen(len(data))
}

// ResourceTools returns lingzhu_list_mcp_resources and lingzhu_read_mcp_resource. They are
// always registered so the model has a stable way to discover non-file data,
// even before a server is configured.
func ResourceTools(manager *Manager) []tool.Tool {
	return []tool.Tool{
		&listResources{manager: manager},
		&readResource{manager: manager},
	}
}

type listResources struct {
	manager *Manager
}

func (t *listResources) Name() string { return "lingzhu_list_mcp_resources" }

func (t *listResources) Description() string {
	return "List documents, records, and other data exposed by MCP servers. Each entry names the server to read it from. Templates are resources addressed by a parameter such as a record ID; fill in the uriTemplate placeholders before reading."
}

func (t *listResources) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "MCP server name as configured. Omit to list every server."}
		}
	}`)
}

func (t *listResources) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_list_mcp_resources: %w", err)
	}
	servers := t.manager.Servers()
	if args.Server != "" {
		found := false
		for _, name := range servers {
			if name == args.Server {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("MCP server %q is not connected", args.Server)
		}
		servers = []string{args.Server}
	}
	if err := call.Allow(ctx, "lingzhu_list_mcp_resources", servers, ""); err != nil {
		return "", err
	}

	var b strings.Builder
	for _, server := range servers {
		items, _ := t.manager.Resources(server)
		if len(items) == 0 {
			continue
		}
		b.WriteString(server + ":\n")
		for _, item := range items {
			if item.URITemplate != "" {
				fmt.Fprintf(&b, "  template %s (%s)", item.Name, item.URITemplate)
			} else {
				fmt.Fprintf(&b, "  %s (%s)", item.Name, item.URI)
			}
			if item.Description != "" {
				b.WriteString(" - " + item.Description)
			}
			if item.MimeType != "" {
				b.WriteString(" [" + item.MimeType + "]")
			}
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "No MCP resources found", nil
	}
	// Stable order is already guaranteed by the server and catalog sorts.
	return strings.TrimRight(b.String(), "\n"), nil
}

type readResource struct {
	manager *Manager
}

func (t *readResource) Name() string { return "lingzhu_read_mcp_resource" }

func (t *readResource) Description() string {
	return "Read one MCP resource by server and URI. Not for local files. Always return the full contents rather than slicing or filtering them; oversized output is truncated automatically."
}

func (t *readResource) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "The server field of the discovered resource."},
			"uri": {"type": "string", "description": "Exact resource URI from discovery, an expanded uriTemplate, or the user."}
		},
		"required": ["server", "uri"]
	}`)
}

func (t *readResource) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Server string `json:"server"`
		URI    string `json:"uri"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_read_mcp_resource: %w", err)
	}
	if args.Server == "" || args.URI == "" {
		return "", fmt.Errorf("lingzhu_read_mcp_resource: server and uri are required")
	}
	if err := call.Allow(ctx, "lingzhu_read_mcp_resource", []string{args.Server + ":" + args.URI}, ""); err != nil {
		return "", err
	}
	contents, err := t.manager.ReadResource(ctx, args.Server, args.URI)
	if err != nil {
		return "", fmt.Errorf("lingzhu_read_mcp_resource: %w", err)
	}
	if len(contents) == 0 {
		return "", nil
	}
	attachResourceContents(call, contents)
	parts := make([]string, 0, len(contents))
	for _, content := range contents {
		parts = append(parts, formatResourceContent(content))
	}
	return strings.Join(parts, "\n"), nil
}

func formatResourceContent(content ResourceContent) string {
	if content.Text != "" {
		return content.Text
	}
	if content.Blob != "" {
		if textualMime(content.MimeType) {
			if data, err := base64.StdEncoding.DecodeString(content.Blob); err == nil {
				return string(data)
			}
		}
		return fmt.Sprintf("[binary resource %s (%s, %d bytes)]", content.URI, content.MimeType, base64Size(content.Blob))
	}
	return ""
}

func textualMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/x-ndjson", "application/x-www-form-urlencoded":
		return true
	}
	return strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml")
}
