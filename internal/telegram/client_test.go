package telegram

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type failingRoundTripper struct {
	err error
}

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func TestClientRedactsBotTokenFromTransportErrors(t *testing.T) {
	t.Parallel()

	const token = "123456:super-secret-bot-token"
	underlying := &http.Client{
		Transport: failingRoundTripper{
			err: errors.New("dial https://api.telegram.org/bot" + token + "/getMe: failed"),
		},
	}
	client := NewClientWithHTTPClient(token, underlying)
	request, err := http.NewRequest(http.MethodGet, "https://api.telegram.org/bot"+token+"/getMe", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	_, err = client.Do(request)
	if err == nil {
		t.Fatal("Do() returned nil error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("transport error leaked bot token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("transport error did not contain redaction marker: %v", err)
	}
}
