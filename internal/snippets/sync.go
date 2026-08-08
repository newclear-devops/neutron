package snippets

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"neutron/internal"
	"neutron/internal/parser"
)

const snippetsDir = "snippets"

// snippetFrontmatter mirrors the YAML frontmatter in each .sh file.
type snippetFrontmatter struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Params      string `yaml:"params"`
}

// SyncSnippets fetches all .sh files from the snippets/ directory of the configured
// GitLab project and replaces the MySQL snippet cache atomically. It returns the
// number of snippets synced.
func SyncSnippets(repoUrl, platform, ref, apiBaseUrl, token string, skipTLS bool, repo *internal.Repository) (int, error) {
	if ref == "" {
		ref = "main"
	}

	projectPath := parser.ExtractGitLabProjectPath(repoUrl)
	if projectPath == "" {
		return 0, fmt.Errorf("snippets: cannot extract project path from repo URL: %s", repoUrl)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLS},
		},
	}

	// 1. List files in snippets/ directory
	encodedProject := url.PathEscape(projectPath)
	listURL := fmt.Sprintf("%s/api/v4/projects/%s/repository/tree?path=%s&ref=%s&per_page=100",
		apiBaseUrl, encodedProject, url.QueryEscape(snippetsDir), url.QueryEscape(ref))

	files, err := fetchFileList(client, listURL, token)
	if err != nil {
		return 0, fmt.Errorf("snippets: list tree: %w", err)
	}

	// 2. Fetch each .sh file and parse
	var snippets []internal.Snippet
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".sh") {
			continue
		}

		rawURL := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s",
			apiBaseUrl, encodedProject, url.PathEscape(f.Path), url.QueryEscape(ref))

		content, err := fetchRawFile(client, rawURL, token)
		if err != nil {
			log.Printf("snippets: WARNING skipping %s: fetch failed: %v", f.Name, err)
			continue
		}

		snippet, err := parseSnippetFile(f.Name, content)
		if err != nil {
			log.Printf("snippets: WARNING skipping %s: parse failed: %v", f.Name, err)
			continue
		}

		snippets = append(snippets, snippet)
	}

	if err := repo.ReplaceAllSnippets(snippets); err != nil {
		return 0, fmt.Errorf("snippets: replace cache: %w", err)
	}

	log.Printf("snippets: synced %d snippets from %s (ref=%s)", len(snippets), repoUrl, ref)
	return len(snippets), nil
}

type treeItem struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
}

func fetchFileList(client *http.Client, urlStr, token string) ([]treeItem, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var items []treeItem
	if err := json.NewDecoder(res.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode tree response: %w", err)
	}

	return items, nil
}

func fetchRawFile(client *http.Client, urlStr, token string) (string, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20)) // 1 MiB
	if err != nil {
		return "", fmt.Errorf("read raw body: %w", err)
	}

	return string(raw), nil
}

// parseSnippetFile extracts metadata and script body from a snippet file.
// Expected format:
//
//	#---
//	# title: My Title
//	# description: Some description
//	# params: FOO, BAR
//	#---
//	#!/bin/sh
//	echo hello
func parseSnippetFile(filename string, content string) (internal.Snippet, error) {
	var fm snippetFrontmatter
	script := content

	if strings.HasPrefix(content, "#---\n") || strings.HasPrefix(content, "#---\r\n") {
		// Normalize line endings
		content = strings.ReplaceAll(content, "\r\n", "\n")

		endIdx := strings.Index(content[5:], "\n#---\n")
		if endIdx < 0 {
			// Try end-of-file closing
			endIdx = strings.Index(content[5:], "\n#---")
		}
		if endIdx >= 0 {
			fmBlock := content[5 : 5+endIdx]
			// Remove the "# " prefix from each line of the YAML block
			var yamlLines []string
			for _, line := range strings.Split(fmBlock, "\n") {
				line = strings.TrimPrefix(line, "# ")
				line = strings.TrimPrefix(line, "#")
				yamlLines = append(yamlLines, line)
			}
			if err := yaml.Unmarshal([]byte(strings.Join(yamlLines, "\n")), &fm); err != nil {
				return internal.Snippet{}, fmt.Errorf("parse frontmatter: %w", err)
			}
			// Script is everything after the closing #---
			scriptStart := 5 + endIdx + len("\n#---\n")
			if scriptStart < len(content) {
				script = content[scriptStart:]
			} else {
				script = ""
			}
		}
	}

	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if name == "" {
		return internal.Snippet{}, fmt.Errorf("empty snippet name from filename %q", filename)
	}

	now := time.Now()
	return internal.Snippet{
		Name:        name,
		Title:       fm.Title,
		Content:     script,
		Description: fm.Description,
		Params:      fm.Params,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}, nil
}
