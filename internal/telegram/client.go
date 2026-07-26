package telegram

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client wraps Telegram's HTTP client and redacts the bot token from transport
// errors. Telegram embeds that token in every request URL, and net/http's
// *url.Error includes the URL in Error(), including inside the bot library's
// own polling logs.
type Client struct {
	httpClient *http.Client
	token      string
}

func NewClient(token string, timeout time.Duration) *Client {
	return NewClientWithHTTPClient(token, &http.Client{Timeout: timeout})
}

func NewClientWithHTTPClient(token string, httpClient *http.Client) *Client {
	return &Client{httpClient: httpClient, token: token}
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("telegram HTTP client is not configured")
	}
	response, err := c.httpClient.Do(req)
	if err == nil {
		return response, nil
	}
	message := err.Error()
	if c.token != "" {
		message = strings.ReplaceAll(message, c.token, "[REDACTED]")
	}
	return response, fmt.Errorf("telegram request failed: %s", message)
}
