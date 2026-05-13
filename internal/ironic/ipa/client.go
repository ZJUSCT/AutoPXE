package ipa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client talks to the ironic-python-agent HTTP API exposed by a deployed node.
type Client struct {
	BaseURL    string // node-reported callback URL, e.g. http://192.168.100.10:9999
	AgentToken string
	HTTP       *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:    baseURL,
		AgentToken: token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) cmdsURL() string {
	return fmt.Sprintf("%s/v1/commands/?agent_token=%s", c.BaseURL, url.QueryEscape(c.AgentToken))
}

func (c *Client) cmdURL(id string) string {
	return fmt.Sprintf("%s/v1/commands/%s?agent_token=%s", c.BaseURL, url.PathEscape(id), url.QueryEscape(c.AgentToken))
}

// SendCommand POSTs a command to the agent and returns the parsed response.
func (c *Client) SendCommand(ctx context.Context, name string, params map[string]any) (map[string]any, error) {
	body := map[string]any{"name": name, "params": params}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cmdsURL(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", name, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	return out, nil
}

// GetCommandStatus fetches the latest status of a previously-issued command.
func (c *Client) GetCommandStatus(ctx context.Context, cmdID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cmdURL(cmdID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get cmd: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode cmd: %w", err)
	}
	return out, nil
}
