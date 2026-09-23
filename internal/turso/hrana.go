package turso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Exec runs one statement with text arguments on the primary at dbURL,
// through its HTTP statement endpoint (/v2/pipeline), and returns the rows it
// affected.
//
// It exists for tursoraft-admin's fence-adopt, which must write the primary
// directly: an operator's machine holds no replica and no group key.
func Exec(ctx context.Context, dbURL, token, sql string, args ...string) (int64, error) {
	base := dbURL
	if rest, ok := strings.CutPrefix(base, "libsql://"); ok {
		base = "https://" + rest
	}
	type value struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	vals := make([]value, len(args))
	for i, a := range args {
		vals[i] = value{Type: "text", Value: a}
	}
	body, err := json.Marshal(map[string]any{
		"requests": []any{
			map[string]any{"type": "execute", "stmt": map[string]any{"sql": sql, "args": vals}},
			map[string]any{"type": "close"},
		},
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, &APIError{Method: http.MethodPost, Path: "/v2/pipeline", Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var out struct {
		Results []struct {
			Type     string `json:"type"`
			Response struct {
				Result struct {
					AffectedRowCount int64 `json:"affected_row_count"`
				} `json:"result"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("turso: decode pipeline response: %w", err)
	}
	if len(out.Results) == 0 {
		return 0, errors.New("turso: the pipeline returned no result")
	}
	first := out.Results[0]
	if first.Type != "ok" {
		msg := "unknown error"
		if first.Error != nil {
			msg = first.Error.Message
		}
		return 0, fmt.Errorf("turso: statement failed: %s", msg)
	}
	return first.Response.Result.AffectedRowCount, nil
}
