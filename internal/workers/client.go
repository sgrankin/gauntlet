package workers

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

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func (c Client) request(ctx context.Context, method, path string, body io.Reader, token string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-Attempt-Token", token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode == 409 {
			return nil, ErrStale
		}
		return nil, fmt.Errorf("worker endpoint returned HTTP %d", response.StatusCode)
	}
	return response, nil
}
func (c Client) post(ctx context.Context, path string, value, output any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	response, err := c.request(ctx, "POST", path, bytes.NewReader(data), "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output != nil && response.StatusCode != 204 {
		return json.NewDecoder(io.LimitReader(response.Body, maxResultBytes+1)).Decode(output)
	}
	return nil
}
