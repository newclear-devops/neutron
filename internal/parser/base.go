package parser

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"net/url"
	"neutron/internal/model"
	"strings"
	"time"
)

const MaxBodySize = 1 << 20 // 1MB

// ErrPipelineNotFound signals that neutron.yaml does not exist in the repository
// (the platform file API returned 404). Callers can detect it with errors.Is to
// fall back to a configured default pipeline. Both GitLab and Codeup return 404
// for a missing file, so this is platform-agnostic.
var ErrPipelineNotFound = errors.New("neutron.yaml not found in repository")

type FileResponse struct {
	Content string `json:"content"`
}

type Base struct {
	AccessApiPath  string
	AccessToken    string
	AuthHeaderName string // e.g. "PRIVATE-TOKEN" (GitLab), "x-yunxiao-token" (Codeup)
	Client         *http.Client
	CodeSha        string
	ReportSha      string
	TargetBranch   string
	Trigger        string
}

func (b *Base) Parse() (model.Pipeline, error) {
	req, err := http.NewRequest("GET", b.AccessApiPath, nil)
	if err != nil {
		return model.Pipeline{}, err
	}
	query := req.URL.Query()
	query.Add("ref", b.CodeSha)
	req.URL.RawQuery = query.Encode()
	authHeader := b.AuthHeaderName
	if authHeader == "" {
		authHeader = "PRIVATE-TOKEN"
	}
	req.Header.Add(authHeader, b.AccessToken)
	res, err := b.Client.Do(req)
	if err != nil {
		return model.Pipeline{}, err
	}
	defer res.Body.Close()
	log.Printf("parser: fetched neutron.yaml GET %s (ref=%s) -> HTTP %d", b.AccessApiPath, b.CodeSha, res.StatusCode)
	if res.StatusCode == http.StatusNotFound {
		log.Printf("parser: neutron.yaml not found (HTTP 404) at %s (ref=%s), signaling default-pipeline fallback", b.AccessApiPath, b.CodeSha)
		return model.Pipeline{}, fmt.Errorf("%w (ref: %s)", ErrPipelineNotFound, b.CodeSha)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return model.Pipeline{}, fmt.Errorf("authentication failed when accessing API (status: %d)", res.StatusCode)
	}
	if res.StatusCode >= 400 {
		return model.Pipeline{}, fmt.Errorf("API returned error (status: %d)", res.StatusCode)
	}
	var fileResponse FileResponse
	err = json.NewDecoder(res.Body).Decode(&fileResponse)
	if err != nil {
		return model.Pipeline{}, err
	}
	neutronContent, err := base64.StdEncoding.DecodeString(fileResponse.Content)
	if err != nil {
		return model.Pipeline{}, err
	}
	var pipeline model.Pipeline
	err = yaml.Unmarshal(neutronContent, &pipeline)
	if err != nil {
		log.Printf("parser: neutron.yaml at %s (ref=%s) failed to parse as YAML: %v", b.AccessApiPath, b.CodeSha, err)
		return pipeline, err
	}
	if len(pipeline.Jobs) == 0 {
		// A HTTP 200 whose body carries no jobs (empty file, error envelope, or a
		// platform that answers a missing file with 200 instead of 404) would
		// otherwise be treated as a valid-but-empty pipeline and silently launch
		// nothing. Log loudly so this is distinguishable from a real 404.
		log.Printf("parser: WARNING neutron.yaml at %s (ref=%s) parsed to 0 jobs from HTTP %d response (%d bytes); default-pipeline fallback will NOT trigger for a non-404 response", b.AccessApiPath, b.CodeSha, res.StatusCode, len(neutronContent))
	} else {
		log.Printf("parser: neutron.yaml at %s (ref=%s) parsed %d job(s)", b.AccessApiPath, b.CodeSha, len(pipeline.Jobs))
	}
	return pipeline, err
}

// ReadBody reads and closes the request body with size limit.
func ReadBody(body io.ReadCloser) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxBodySize))
	body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading webhook body: %w", err)
	}
	return data, nil
}

// BuildSourceUrl constructs the URL to the source branch or MR on the code hosting platform.
// It returns empty string for unsupported triggers (e.g., "API").
func BuildSourceUrl(platform, trigger, codebaseUrl, repoUrl, ref, codeSha string, mrIid int) string {
	switch platform {
	case "GitLab":
		projectPath := ExtractGitLabProjectPath(repoUrl)
		if projectPath == "" {
			return ""
		}
		refName := ExtractRefName(ref)
		switch trigger {
		case "MR":
			return fmt.Sprintf("%s/%s/-/merge_requests/%d", codebaseUrl, projectPath, mrIid)
		case "PUSH":
			return fmt.Sprintf("%s/%s/-/tree/%s", codebaseUrl, projectPath, refName)
		case "TAG":
			return fmt.Sprintf("%s/%s/-/tags/%s", codebaseUrl, projectPath, refName)
		}
	case "Codeup":
		orgId, codeupProject := ExtractCodeupOrgAndProject(repoUrl)
		if orgId == "" || codeupProject == "" {
			return ""
		}
		// codeupProject already contains orgId as its first segment (e.g. "orgId/group/project")
		projectUrl := fmt.Sprintf("%s/codeup/%s", codebaseUrl, codeupProject)
		refName := ExtractRefName(ref)
		switch trigger {
		case "MR":
			return fmt.Sprintf("%s/change/%d", projectUrl, mrIid)
		case "PUSH":
			return fmt.Sprintf("%s/commit/%s?branch=%s", projectUrl, codeSha, url.QueryEscape(refName))
		case "TAG":
			return fmt.Sprintf("%s/tree/%s", projectUrl, refName)
		}
	}
	return ""
}

func ExtractRefName(ref string) string {
	if after, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return after
	}
	return ref
}

// DetectTrigger returns trigger type, ref SHA, report SHA, target branch from common webhook fields.
func DetectTrigger(webhookType, codeSha, lastCommitId, targetBranch string) (trigger, ref, reportSha, tBranch string, err error) {
	switch webhookType {
	case "merge_request":
		return "MR", lastCommitId, lastCommitId, targetBranch, nil
	case "tag_push":
		return "TAG", codeSha, codeSha, "", nil
	case "push":
		return "PUSH", codeSha, codeSha, "", nil
	default:
		return "", "", "", "", fmt.Errorf("unsupported webhook type: %s", webhookType)
	}
}

// FetchPipeline fetches neutron.yaml from a repository at the given ref.
// It constructs the platform-specific API path from the repo URL.
func FetchPipeline(platform, repoUrl, ref string, codebaseUrl, codebaseToken string, skipTLS bool) (model.Pipeline, error) {
	var accessApiPath, authHeader string

	switch platform {
	case "GitLab":
		projectPath := ExtractGitLabProjectPath(repoUrl)
		if projectPath == "" {
			return model.Pipeline{}, fmt.Errorf("cannot extract project path from URL: %s", repoUrl)
		}
		encodedPath := url.PathEscape(projectPath)
		accessApiPath = fmt.Sprintf("%s/api/v4/projects/%s/repository/files/neutron.yaml", codebaseUrl, encodedPath)
		authHeader = "PRIVATE-TOKEN"
	case "Codeup":
		orgId, projectPath := ExtractCodeupOrgAndProject(repoUrl)
		if orgId == "" || projectPath == "" {
			return model.Pipeline{}, fmt.Errorf("cannot extract org-id and project path from URL: %s", repoUrl)
		}
		encodedProjectPath := EncodeCodeupProjectPath(projectPath)
		accessApiPath = fmt.Sprintf("%s/oapi/v1/codeup/organizations/%s/repositories/%s/files/neutron.yaml", codebaseUrl, orgId, encodedProjectPath)
		authHeader = "x-yunxiao-token"
	default:
		return model.Pipeline{}, fmt.Errorf("unsupported platform: %s", platform)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLS},
		},
	}

	base := Base{
		AccessApiPath:  accessApiPath,
		AccessToken:    codebaseToken,
		AuthHeaderName: authHeader,
		Client:         client,
		CodeSha:        ref,
	}

	return base.Parse()
}
