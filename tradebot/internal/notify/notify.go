// Package notify posts run summaries to a Slack-compatible incoming webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

type Notifier struct{ url string }

// New returns a notifier; it is a no-op when the env var is unset.
func New(envVar string) *Notifier { return &Notifier{url: os.Getenv(envVar)} }

func (n *Notifier) Send(ctx context.Context, text string) error {
	if n == nil || n.url == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
