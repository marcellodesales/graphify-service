// Package mcpproxy is a tiny client for the graphify-mcp Streamable HTTP server
// (PRD-004). The server runs stateless with --json-response, so a single
// JSON-RPC tools/call works with no initialize handshake — no MCP SDK needed.
// The API injects project_path per call so one shared server answers for any
// repo on the shared volume (the composition/reverse-proxy pattern).
package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls a graphify-mcp Streamable HTTP endpoint.
type Client struct {
	url string
	hc  *http.Client
}

// New returns a Client for the given /mcp URL.
func New(url string) *Client {
	return &Client{url: url, hc: &http.Client{Timeout: 30 * time.Second}}
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

type rpcResponse struct {
	Result *toolResult `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// CallTool invokes an MCP tool and returns its concatenated text content and the
// tool-level isError flag. A transport/protocol failure returns a non-nil error.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (text string, isErr bool, err error) {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		return "", false, fmt.Errorf("mcpproxy: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return "", false, fmt.Errorf("mcpproxy: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("mcpproxy: call %s: %w", name, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("mcpproxy: http %d: %s", resp.StatusCode, snippet(data))
	}
	var r rpcResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", false, fmt.Errorf("mcpproxy: decode: %w (body=%s)", err, snippet(data))
	}
	if r.Error != nil {
		return "", false, fmt.Errorf("mcpproxy: rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	if r.Result == nil {
		return "", false, fmt.Errorf("mcpproxy: empty result")
	}
	var sb strings.Builder
	for _, part := range r.Result.Content {
		if part.Type == "text" {
			sb.WriteString(part.Text)
		}
	}
	return sb.String(), r.Result.IsError, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
