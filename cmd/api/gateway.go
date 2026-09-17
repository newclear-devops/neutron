package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// External inputs for the manual-trigger toolbar on the project page.
//
// The browser cannot call the codebase gateway or the dependency service
// directly (different origin, and both live on an internal network), so the
// API server proxies the three read-only lookups the toolbar needs. Neutron
// itself is the origin of both the SPA and these endpoints, so the trigger
// itself goes straight to POST /api/trigger and needs no proxy here.

const (
	upstreamTimeout = 30 * time.Second
	maxUpstreamBody = 10 << 20 // 10 MiB

	refKindBranches = "branches"
	refKindTags     = "tags"
)

// refItem is one selectable entry of a branch/tag list. Only name is required:
// default lets the UI preselect the repository's default branch, and commit
// (a short SHA) gives the option some context when the gateway reports one.
type refItem struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// upstreamClient builds an HTTP client for one of the optional upstream
// services, each of which carries its own skip_tls_verify switch because they
// commonly sit behind self-signed certificates.
func upstreamClient(skipTLSVerify bool) *http.Client {
	return &http.Client{
		Timeout: upstreamTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify},
		},
	}
}

// refListURL builds the gateway URL for a repository's branch or tag list. The
// SSH URL is a single path segment, so PathEscape is applied to the whole
// value (slashes included) rather than segment by segment.
func refListURL(base, sshURL, kind string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	sshURL = strings.TrimSpace(sshURL)
	if base == "" || sshURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/api/v1/projects/%s/repository/%s", base, url.PathEscape(sshURL), kind)
}

// handleProjectBranches lists the registered project's branches.
// GET /api/projects/:id/branches
func (s *Server) handleProjectBranches(c *gin.Context) {
	s.handleProjectRefs(c, refKindBranches)
}

// handleProjectTags lists the registered project's tags.
// GET /api/projects/:id/tags
func (s *Server) handleProjectTags(c *gin.Context) {
	s.handleProjectRefs(c, refKindTags)
}

// handleProjectRefs resolves the project's registered repo URL server-side and
// proxies the gateway's branch/tag list for it. Deriving the URL from the
// project id — rather than accepting one from the caller — keeps the client
// from turning Neutron into a pass-through for arbitrary repositories.
func (s *Server) handleProjectRefs(c *gin.Context, kind string) {
	if s.config.Gateway.Url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "codebase gateway is not configured (set gateway.url or NEUTRON_GITREPO_GATEWAY_URL)"})
		return
	}
	project := s.repo.GetWebhookConfig(c.Param("id"))
	if project.Id == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	target := refListURL(s.config.Gateway.Url, project.RepoUrl, kind)
	if target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot derive a repository path from " + project.RepoUrl})
		return
	}

	body, status, err := upstreamGet(c.Request.Context(), upstreamClient(s.config.Gateway.SkipTLSVerify), target)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "codebase gateway request failed: " + err.Error()})
		return
	}
	if status != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("codebase gateway returned HTTP %d: %s", status, truncateBody(body, 512))})
		return
	}
	items, err := parseRefItems(body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected codebase gateway response: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{kind: items})
}

// handleDependencyBranches lists the shared dependency branches.
// GET /api/dependency/branches
func (s *Server) handleDependencyBranches(c *gin.Context) {
	if s.config.Dependency.Url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dependency service is not configured (set dependency.url or NEUTRON_DEPENDENCY_URL)"})
		return
	}
	target := strings.TrimSuffix(strings.TrimSpace(s.config.Dependency.Url), "/") + "/api/v1/branches"

	body, status, err := upstreamGet(c.Request.Context(), upstreamClient(s.config.Dependency.SkipTLSVerify), target)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "dependency service request failed: " + err.Error()})
		return
	}
	if status != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("dependency service returned HTTP %d: %s", status, truncateBody(body, 512))})
		return
	}
	c.JSON(http.StatusOK, gin.H{"branches": parseDependencyBranches(body)})
}

// handleDependencyDefault reports the dependency service's currently selected
// branch, so the toolbar can preselect it.
// GET /api/dependency/default
func (s *Server) handleDependencyDefault(c *gin.Context) {
	if s.config.Dependency.Url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dependency service is not configured (set dependency.url or NEUTRON_DEPENDENCY_URL)"})
		return
	}
	target := strings.TrimSuffix(strings.TrimSpace(s.config.Dependency.Url), "/") + "/property"

	body, status, err := upstreamGet(c.Request.Context(), upstreamClient(s.config.Dependency.SkipTLSVerify), target)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "dependency service request failed: " + err.Error()})
		return
	}
	if status != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("dependency service returned HTTP %d: %s", status, truncateBody(body, 512))})
		return
	}
	c.JSON(http.StatusOK, gin.H{"branch": parseDepBranch(body)})
}

// upstreamGet performs a GET and returns the body together with the status
// code. A non-200 status is deliberately not an error: each caller maps it to
// a response of its own.
func upstreamGet(ctx context.Context, client *http.Client, target string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseRefItems decodes a gateway branch/tag list. The gateway fronts two
// platforms that answer in different shapes, so both a bare array and the
// common envelopes ({"data": ...}, {"branches": ...}, {"tags": ...}) are
// accepted, and each entry may be a plain name or an object.
func parseRefItems(body []byte) ([]refItem, error) {
	var probe interface{}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	entries := unwrapList(probe)
	items := make([]refItem, 0, len(entries))
	for _, entry := range entries {
		item := refItemFrom(entry)
		if item.Name != "" {
			items = append(items, item)
		}
	}
	return items, nil
}

// unwrapList digs the actual array out of the envelopes the upstream services
// are known to use. Returns nil when the payload carries no list at all.
func unwrapList(v interface{}) []interface{} {
	switch t := v.(type) {
	case []interface{}:
		return t
	case map[string]interface{}:
		for _, key := range []string{"data", "branches", "tags", "items", "list"} {
			if inner, ok := t[key]; ok {
				if list := unwrapList(inner); list != nil {
					return list
				}
			}
		}
	}
	return nil
}

// refItemFrom reads one list entry, tolerating both "name" and the
// platform-specific spellings the gateway may pass through.
func refItemFrom(v interface{}) refItem {
	if name, ok := v.(string); ok {
		return refItem{Name: name}
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return refItem{}
	}
	item := refItem{
		Name:    firstString(obj, "name", "branch", "tag", "ref", "branch_name", "tag_name"),
		Default: firstBool(obj, "default", "is_default", "isDefault"),
	}
	if commit, ok := obj["commit"].(map[string]interface{}); ok {
		item.Commit = firstString(commit, "short_id", "shortId", "id", "sha")
	}
	return item
}

// parseDependencyBranches reduces the dependency service's branch list to the
// names the dropdown needs.
func parseDependencyBranches(body []byte) []string {
	var probe interface{}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	var names []string
	for _, entry := range unwrapList(probe) {
		switch t := entry.(type) {
		case string:
			if t != "" {
				names = append(names, t)
			}
		case map[string]interface{}:
			if name := firstString(t, "name", "branch", "branchName"); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// parseDepBranch reads the depBranch field out of the dependency service's
// /property payload, which is a flat key=value text file:
//
//	depBranch=master202609
//	other=value
func parseDepBranch(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "depBranch="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstString(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if s, ok := obj[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstBool(obj map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if b, ok := obj[key].(bool); ok {
			return b
		}
	}
	return false
}

// truncateBody keeps error messages readable when an upstream service answers
// with an HTML error page instead of JSON. Truncation is rune-based so a
// multi-byte character is never cut in half.
func truncateBody(body []byte, limit int) string {
	s := strings.TrimSpace(string(body))
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}
