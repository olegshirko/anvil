package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const apiBase = "https://api.github.com"

// Client is a thin HTTP wrapper for a subset of the GitHub API.
type Client struct {
	token      string
	httpClient *http.Client
}

// NewClient creates a new GitHub API client.
func NewClient(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) newRequest(method, url string, body interface{}) (*http.Request, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

func (c *Client) do(req *http.Request, out interface{}) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		var ghErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ghErr)
		return fmt.Errorf("github API %s: %s (message: %s)", resp.Status, req.URL, ghErr.Message)
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Issue represents a GitHub issue.
type Issue struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
}

// CreateIssueRequest is the payload for creating an issue.
type CreateIssueRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// CreateIssue creates a new issue in the given repository.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, req CreateIssueRequest) (*Issue, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues", apiBase, owner, repo)
	httpReq, err := c.newRequest(http.MethodPost, url, req)
	if err != nil {
		return nil, err
	}
	httpReq = httpReq.WithContext(ctx)

	var issue Issue
	if err := c.do(httpReq, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// TriggerWorkflowDispatch triggers a workflow_dispatch event.
func (c *Client) TriggerWorkflowDispatch(ctx context.Context, owner, repo, workflowID, ref string, inputs map[string]string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches", apiBase, owner, repo, workflowID)
	payload := map[string]interface{}{
		"ref":    ref,
		"inputs": inputs,
	}
	httpReq, err := c.newRequest(http.MethodPost, url, payload)
	if err != nil {
		return err
	}
	httpReq = httpReq.WithContext(ctx)
	return c.do(httpReq, nil)
}

// ReleaseAsset represents a single asset in a GitHub release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Release represents a GitHub release.
type Release struct {
	TagName string         `json:"tag_name"`
	Name    string         `json:"name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ListReleases returns releases for the given repository.
func (c *Client) ListReleases(ctx context.Context, owner, repo string) ([]Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases", apiBase, owner, repo)
	httpReq, err := c.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq = httpReq.WithContext(ctx)

	var releases []Release
	if err := c.do(httpReq, &releases); err != nil {
		return nil, err
	}
	return releases, nil
}

// GetReleaseByTag returns a specific release by tag name.
func (c *Client) GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", apiBase, owner, repo, tag)
	httpReq, err := c.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq = httpReq.WithContext(ctx)

	var release Release
	if err := c.do(httpReq, &release); err != nil {
		return nil, err
	}
	return &release, nil
}
