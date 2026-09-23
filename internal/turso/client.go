// Package turso is the operator's client for the Turso platform API and for a
// primary's HTTP statement endpoint.
//
// Its only importer is cmd/tursoraft-admin. A Manager never imports it: an
// endpoint opens every shared database from the url and token in its signed
// document and holds no organisation token (manager's
// TestManagerDoesNotImportThePlatformClient keeps it so).
package turso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the platform API.
const DefaultBaseURL = "https://api.turso.tech"

// Errors.
var (
	// ErrNotFound means the platform has no database by that name.
	ErrNotFound = errors.New("turso: database not found")
	// ErrWrongGroup means a database by that name exists in another group.
	ErrWrongGroup = errors.New("turso: database exists in another group")
)

// APIError is a non-2xx platform answer. Body is the platform's own
// explanation, which is what an operator needs to act on a 403.
type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("turso: %s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Body)
}

// Client calls the platform API for one organisation.
type Client struct {
	Org     string
	Token   string // organisation token; never logged
	BaseURL string
	HTTP    *http.Client
}

// Database is a database as the platform describes it. Only documented
// fields are read.
type Database struct {
	Name     string `json:"Name"`
	DbID     string `json:"DbId"`
	Hostname string `json:"Hostname"`
	Group    string `json:"group"`
}

// URL is the primary's libSQL URL.
func (d Database) URL() string { return "libsql://" + d.Hostname }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("turso: %s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *Client) orgPath(parts ...string) string {
	p := "/v1/organizations/" + url.PathEscape(c.Org)
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// GetDatabase returns the named database, or ErrNotFound.
func (c *Client) GetDatabase(ctx context.Context, name string) (Database, error) {
	var out struct {
		Database Database `json:"database"`
	}
	err := c.do(ctx, http.MethodGet, c.orgPath("databases", name), nil, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return Database{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return out.Database, err
}

// CreateDatabase creates the named database in group.
func (c *Client) CreateDatabase(ctx context.Context, name, group string) (Database, error) {
	var out struct {
		Database Database `json:"database"`
	}
	err := c.do(ctx, http.MethodPost, c.orgPath("databases"), map[string]string{"name": name, "group": group}, &out)
	if err == nil && out.Database.Group == "" {
		out.Database.Group = group
	}
	return out.Database, err
}

// EnsureDatabase returns the named database, creating it in group when it
// does not exist, and refuses one that exists in another group: a name is
// unique in the organisation, so the database found there is not the one the
// document means.
func (c *Client) EnsureDatabase(ctx context.Context, name, group string) (db Database, created bool, err error) {
	db, err = c.GetDatabase(ctx, name)
	switch {
	case err == nil:
		if db.Group != group {
			return Database{}, false, fmt.Errorf("%w: %s is in group %q, not %q", ErrWrongGroup, name, db.Group, group)
		}
		return db, false, nil
	case errors.Is(err, ErrNotFound):
		db, err = c.CreateDatabase(ctx, name, group)
		return db, err == nil, err
	default:
		return Database{}, false, err
	}
}

// CreateToken mints a token scoped to one database. expiration is the
// platform's form ("never", "2w", ...); readOnly limits it to reads.
func (c *Client) CreateToken(ctx context.Context, name, expiration string, readOnly bool) (string, error) {
	q := url.Values{}
	if expiration != "" {
		q.Set("expiration", expiration)
	}
	if readOnly {
		q.Set("authorization", "read-only")
	} else {
		q.Set("authorization", "full-access")
	}
	var out struct {
		JWT string `json:"jwt"`
	}
	if err := c.do(ctx, http.MethodPost, c.orgPath("databases", name, "auth", "tokens")+"?"+q.Encode(), nil, &out); err != nil {
		return "", err
	}
	if out.JWT == "" {
		return "", errors.New("turso: the platform returned no token")
	}
	return out.JWT, nil
}
