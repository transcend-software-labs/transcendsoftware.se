// Package imagegen generates and edits images via the OpenAI Images API
// (gpt-image-2 by default), for the "Generate with AI" content slots. The
// customer generates a few candidates, picks one, then edits that same image
// iteratively — so Edit takes the chosen image as a reference rather than
// starting over.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
)

// Client talks to an OpenAI-compatible Images API.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	size    string
	http    *http.Client
}

// New returns a client. size defaults to 1024x1024 when empty.
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: baseURL, apiKey: apiKey, model: model, size: "1024x1024",
		http: &http.Client{Timeout: 3 * time.Minute}, // image gen is slow
	}
}

type genResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Generate returns up to n freshly generated PNG images for the prompt.
func (c *Client) Generate(ctx context.Context, prompt string, n int) ([][]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"model": c.model, "prompt": prompt, "n": n, "size": c.size,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.apiKey)
	return c.do(req)
}

// Edit returns up to n images derived from the reference PNG per the prompt —
// keeping its composition while applying the requested change.
func (c *Client) Edit(ctx context.Context, image []byte, prompt string, n int) ([][]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", c.model)
	_ = mw.WriteField("prompt", prompt)
	_ = mw.WriteField("n", strconv.Itoa(n))
	_ = mw.WriteField("size", c.size)
	fw, err := mw.CreateFormFile("image", "image.png")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(image); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/images/edits", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", mw.FormDataContentType())
	req.Header.Set("authorization", "Bearer "+c.apiKey)
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([][]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imagegen: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed genResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("imagegen: decode (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("imagegen: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("imagegen: no images (status %d)", resp.StatusCode)
	}
	out := make([][]byte, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.B64JSON == "" {
			continue
		}
		png, err := base64.StdEncoding.DecodeString(d.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("imagegen: bad base64: %w", err)
		}
		out = append(out, png)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("imagegen: empty image data")
	}
	return out, nil
}
