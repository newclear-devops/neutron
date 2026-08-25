package main

import (
	"encoding/json"
	"fmt"
	"log"

	"neutron/internal"
	"neutron/internal/ccwork"
	"neutron/internal/model"
)

// sendJobNotifications fans a title/content message out to a job's configured
// notification targets: IM personal messages for each user, and CCWork group
// robot webhooks for each group URL. Each send runs in its own goroutine,
// preserving fire-and-forget semantics. A nil or empty Notify sends nothing.
// Failures are logged, never propagated to the caller.
func (s *Server) sendJobNotifications(n *model.Notify, title, content string) {
	if n == nil {
		return
	}
	if len(n.Users) > 0 {
		if s.notifyClient == nil {
			log.Printf("notify: job has %d IM user recipient(s) but the notify client is disabled — set notify url/corp_id/app_id in config", len(n.Users))
		} else {
			for _, u := range n.Users {
				go func(userId string) {
					if err := s.notifyClient.SendMessage(userId, title, content); err != nil {
						log.Printf("notify: failed to send to user %s: %v", userId, err)
					}
				}(u)
			}
		}
	}
	if len(n.Groups) > 0 {
		ccWebhooks := make([]ccwork.Webhook, len(n.Groups))
		for i, url := range n.Groups {
			ccWebhooks[i] = ccwork.Webhook{Url: url}
		}
		go s.ccworkRobot.SendToAll(ccWebhooks, title, content)
	}
}

// notifyJobCompleted sends the completion notification for a job to the
// targets persisted on its DB row. failed selects the failure/success title;
// reason, when non-empty and the job failed, is appended to the message body
// (the failing step for runner-reported jobs, the K8s failure condition for
// reconciled jobs). Callers that already hold the job row pass it in to avoid
// a duplicate lookup.
func (s *Server) notifyJobCompleted(dbJob *internal.PipelineJob, failed bool, reason string) {
	if dbJob == nil {
		return
	}
	var status internal.JobStatus
	_ = json.Unmarshal([]byte(dbJob.Status), &status)
	statusUrl := fmt.Sprintf("%s/#/status/%s", s.config.Host, dbJob.Name)
	project := s.repo.GetWebhookConfig(dbJob.ProjectId)
	repoUrl := project.RepoUrl
	if repoUrl == "" {
		repoUrl = dbJob.ProjectId
	}
	title, content := completionMessage(repoUrl, dbJob.Name, status.SourceUrl, statusUrl, failed, reason)
	s.sendJobNotifications(parseNotify(dbJob.Notify), title, content)
}

// completionMessage builds the completion notification title and body. The
// failure reason is only rendered for failed jobs, so a successful final
// report that carries a non-empty description never prints a spurious
// "原因" line.
func completionMessage(repoUrl, jobName, sourceUrl, statusUrl string, failed bool, reason string) (string, string) {
	var title string
	if failed {
		title = "❌ 流水线执行失败"
	} else {
		title = "✅ 流水线执行成功"
	}
	content := fmt.Sprintf("📂 项目: %s\n📋 任务: %s\n🔗 查看: %s", repoUrl, jobName, statusUrl)
	if sourceUrl != "" {
		content += fmt.Sprintf("\n📎 源码: %s", sourceUrl)
	}
	if failed && reason != "" {
		content += fmt.Sprintf("\n📝 原因: %s", reason)
	}
	return title, content
}
