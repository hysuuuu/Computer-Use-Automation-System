package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// OpenAIClient implements the planner.LLMClient interface using the OpenAI Chat Completions API.
// It can also be used with drop-in replacements like Anthropic (via bridge), Groq, or vLLM
// by overriding the BaseURL.
type OpenAIClient struct {
	BaseURL string
	APIKey  string
	Model   string
	client  *http.Client
}

// NewOpenAIClient creates a new OpenAI client.
// It reads the OPENAI_API_KEY environment variable.
// Default model is gpt-4o.
func NewOpenAIClient() *OpenAIClient {
	apiKey := os.Getenv("OPENAI_API_KEY")
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o"
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	return &OpenAIClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		client:  &http.Client{},
	}
}

// Complete sends a prompt to the OpenAI API and returns the text response.
// This fulfills the planner.LLMClient interface.
func (c *OpenAIClient) Complete(ctx context.Context, prompt string) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("OpenAIClient: OPENAI_API_KEY is not set")
	}

	reqBody := map[string]interface{}{
		"model": c.Model,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature": 0.0, // We want deterministic JSON capability outputs
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenAI API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("OpenAI API returned no choices")
	}

	return response.Choices[0].Message.Content, nil
}
