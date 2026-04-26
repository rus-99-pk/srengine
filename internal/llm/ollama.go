package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rus-99-pk/srengine/internal/agent"
	"github.com/rus-99-pk/srengine/internal/config"
)

type OllamaProvider struct {
	cfg    config.LLMConfig
	client *http.Client
}

func NewOllamaProvider(cfg config.LLMConfig) (*OllamaProvider, error) {
	return &OllamaProvider{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (p *OllamaProvider) Name() string {
	return fmt.Sprintf("ollama/%s", p.cfg.Model)
}

// ollamaRequest is the POST body for Ollama /api/chat.
type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []agent.Message `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx"`
}

// ollamaResponse is the non-streaming response from Ollama /api/chat.
type ollamaResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

// Complete sends messages to Ollama and returns the assistant reply.
func (p *OllamaProvider) Complete(ctx context.Context, messages []agent.Message) (string, error) {
	reqBody := ollamaRequest{
		Model:    p.cfg.Model,
		Messages: messages,
		Stream:   false,
		Options: ollamaOptions{
			Temperature: 0.1, // low temperature for deterministic JSON output
			NumCtx:      4096,
		},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.OllamaURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var ollamaResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return ollamaResp.Message.Content, nil
}