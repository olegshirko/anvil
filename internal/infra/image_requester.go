package infra

import (
	"context"
	"fmt"

	"anvil/internal/usecase"
	"anvil/internal/util/github"

	log "github.com/sirupsen/logrus"
)

// GitHubImageRequester creates issues and triggers workflows via the GitHub API.
type GitHubImageRequester struct {
	client *github.Client
}

// NewGitHubImageRequester creates a new requester.
func NewGitHubImageRequester(client *github.Client) usecase.ImageRequester {
	return &GitHubImageRequester{client: client}
}

// Request implements usecase.ImageRequester.
func (r *GitHubImageRequester) Request(ctx context.Context, reqType usecase.ImageRequestType, imageRuntime, arch, tag, version string, wait bool) (*usecase.ImageRequestResult, error) {
	var title, body string

	switch reqType {
	case usecase.RequestVMBaseImage:
		title = fmt.Sprintf("[anvil] Request VM base image: ubuntu-%s-minimal-cloudimg-%s-%s.qcow2", version, arch, imageRuntime)
		body = fmt.Sprintf(
			"<!-- anvil-request-image -->\n\n**Request Type:** VM base image\n**Runtime:** %s\n**Arch:** %s\n**Tag:** %s\n\nThis request was generated automatically by anvil.",
			imageRuntime, arch, tag,
		)
	case usecase.RequestDockerImage:
		title = fmt.Sprintf("[anvil] Request Docker image: %s:%s (%s)", imageRuntime, tag, arch)
		body = fmt.Sprintf(
			"<!-- anvil-request-image\n{\"image\":\"%s\",\"tag\":\"%s\",\"platform\":\"%s\"}\n-->\n\n**Request Type:** Docker image\n**Image:** %s\n**Tag:** %s\n**Platform:** %s\n\nThis request was generated automatically by anvil.\n\nThe reconcile workflow will process this issue and pull the image from Docker Hub.",
			imageRuntime, tag, arch,
			imageRuntime, tag, arch,
		)
	default:
		return nil, fmt.Errorf("unknown request type: %s", reqType)
	}

	issue, err := r.client.CreateIssue(ctx, "olegshirko", "docker-mirror", github.CreateIssueRequest{
		Title: title,
		Body:  body,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create issue: %w", err)
	}

	log.Infof("Created issue: %s", issue.HTMLURL)

	result := &usecase.ImageRequestResult{
		IssueNumber: issue.Number,
		IssueURL:    issue.HTMLURL,
	}

	return result, nil
}
