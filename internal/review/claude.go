package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"

// AnalyzeRequest holds the parameters for a single Claude API call.
type AnalyzeRequest struct {
	APIKey  string
	Model   string
	System  string
	Message string
}

// Analyze calls the real Claude API when APIKey is set, otherwise falls back
// to the mock which prints the full prompt to stdout.
func Analyze(ctx context.Context, req AnalyzeRequest) (string, int, int, error) {
	if req.APIKey == "" {
		return analyzeMock(req)
	}
	return analyzeReal(ctx, req)
}

// ── Real client ───────────────────────────────────────────────────────────────

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func analyzeReal(ctx context.Context, req AnalyzeRequest) (string, int, int, error) {
	body := anthropicRequest{
		Model:     req.Model,
		MaxTokens: 8192,
		System:    req.System,
		Messages:  []anthropicMessage{{Role: "user", Content: req.Message}},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", req.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", 0, 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("read response: %w", err)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return "", 0, 0, fmt.Errorf("unmarshal response (status=%d): %w\nbody: %s", resp.StatusCode, err, respBody)
	}

	if ar.Error != nil {
		return "", 0, 0, fmt.Errorf("API error (%s): %s", ar.Error.Type, ar.Error.Message)
	}

	for _, c := range ar.Content {
		if c.Type == "text" {
			return c.Text, ar.Usage.InputTokens, ar.Usage.OutputTokens, nil
		}
	}

	return "", 0, 0, fmt.Errorf("no text content in response (status=%d)", resp.StatusCode)
}

// ── Mock client ───────────────────────────────────────────────────────────────

const divider = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

func analyzeMock(req AnalyzeRequest) (string, int, int, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n", divider)
	fmt.Fprintf(&b, "MODEL: %s   [mock — ANTHROPIC_API_KEY not set]\n", req.Model)
	fmt.Fprintf(&b, "%s\n\n", divider)

	fmt.Fprintf(&b, "SYSTEM PROMPT\n%s\n", divider)
	fmt.Fprintf(&b, "%s\n\n", req.System)

	fmt.Fprintf(&b, "USER MESSAGE\n%s\n", divider)
	fmt.Fprintf(&b, "%s\n", req.Message)

	fmt.Fprintf(&b, "%s\n", divider)
	fmt.Fprintf(&b, "[mock] set ANTHROPIC_API_KEY to send this to Claude\n")
	fmt.Fprintf(&b, "%s\n", divider)

	return b.String(), 0, 0, nil
}
