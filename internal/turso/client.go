package turso

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TursoConfig represents Turso Platform API configuration (local copy to avoid import cycle).
type TursoConfig struct {
	APIToken string `json:"api_token"`
	OrgName  string `json:"org_name"`
	BaseURL  string `json:"base_url"`
}

// TursoClient provides methods to interact with the Turso Platform API
type TursoClient struct {
	config     TursoConfig
	httpClient *http.Client
}

// NewTursoClient creates a new Turso API client
func NewTursoClient(cfg TursoConfig) *TursoClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.turso.tech"
	}

	return &TursoClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Database represents a Turso database
type Database struct {
	Name       string `json:"name"`
	Group      string `json:"group"`
	PrimaryURL string `json:"primary_url"`
	Hostname   string `json:"hostname"`
}

// Group represents a Turso group
type Group struct {
	Name      string     `json:"name"`
	Primary   string     `json:"primary"`
	Locations []string   `json:"locations"`
	Databases []Database `json:"databases"`
}

// makeRequest makes an authenticated request to the Turso API
func (tc *TursoClient) makeRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody strings.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = *strings.NewReader(string(jsonBody))
	}

	url := tc.config.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, &reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tc.config.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}

	return resp, nil
}

// getGroup retrieves information about a specific group
func (tc *TursoClient) getGroup(ctx context.Context, groupName string) (*Group, error) {
	resp, err := tc.makeRequest(ctx, "GET", fmt.Sprintf("/v1/organizations/%s/groups/%s", tc.config.OrgName, groupName), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("group %s not found", groupName)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result struct {
		Group Group `json:"group"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result.Group, nil
}

// createDatabase creates a new database in the specified group
func (tc *TursoClient) createDatabase(ctx context.Context, groupName, dbName string) (*Database, error) {
	reqBody := map[string]interface{}{
		"name":  dbName,
		"group": groupName,
	}

	resp, err := tc.makeRequest(ctx, "POST", fmt.Sprintf("/v1/organizations/%s/databases", tc.config.OrgName), reqBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result struct {
		Database Database `json:"database"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result.Database, nil
}

// getDatabase retrieves information about a specific database
func (tc *TursoClient) getDatabase(ctx context.Context, dbName string) (*Database, error) {
	resp, err := tc.makeRequest(ctx, "GET", fmt.Sprintf("/v1/organizations/%s/databases/%s", tc.config.OrgName, dbName), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("database %s not found", dbName)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result struct {
		Database Database `json:"database"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result.Database, nil
}
