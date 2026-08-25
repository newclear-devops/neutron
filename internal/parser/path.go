package parser

import (
	"net/url"
	"strings"
)

// ExtractGitLabProjectPath extracts the project path from a GitLab SSH or HTTP URL.
// e.g. "git@gitlab.example.com:group/project.git" → "group/project"
// e.g. "ssh://git@gitlab.example.com:22/group/project.git" → "group/project"
// e.g. "http://gitlab.example.com/group/project.git" → "group/project"
func ExtractGitLabProjectPath(repoUrl string) string {
	// Handle SCP-like syntax: git@host:path.git
	if strings.HasPrefix(repoUrl, "git@") {
		idx := strings.Index(repoUrl, ":")
		if idx > 0 {
			path := repoUrl[idx+1:]
			return strings.TrimSuffix(path, ".git")
		}
	}

	// Handle standard URL (ssh:// or http://)
	parsed, err := url.Parse(repoUrl)
	if err != nil {
		return ""
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	return strings.TrimSuffix(path, ".git")
}

// ExtractCodeupOrgAndProject extracts org-id and project path from a Codeup SSH URL.
// e.g. "ssh://git@codeup.devops.csdc.com:9022/org-id/SZ/PublicService/repo.git"
// → orgId="org-id", projectPath="org-id/SZ/PublicService/repo"
func ExtractCodeupOrgAndProject(sshUrl string) (orgId, projectPath string) {
	parsed, err := url.Parse(sshUrl)
	if err != nil {
		return "", ""
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")

	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", ""
	}

	orgId = parts[0]
	projectPath = strings.Join(parts, "/")
	return orgId, projectPath
}

// EncodeCodeupProjectPath encodes a project path for Codeup API using %252F.
// e.g. "org-id/SZ/PublicService/repo" → "org-id%252FSZ%252FPublicService%252Frepo"
func EncodeCodeupProjectPath(projectPath string) string {
	return strings.ReplaceAll(projectPath, "/", "%252F")
}

// ExtractRepoName extracts the leaf repository name from a Git SSH/HTTP URL.
// Mirrors the frontend getRepoName(): strip the ".git" suffix, take the last
// path segment. e.g. "git@gitlab.example.com:group/project.git" → "project".
func ExtractRepoName(repoUrl string) string {
	trimmed := strings.TrimSuffix(repoUrl, ".git")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	return trimmed
}
