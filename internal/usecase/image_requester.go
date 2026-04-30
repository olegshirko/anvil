package usecase

import "context"

// ImageRequestType distinguishes what kind of image is being requested.
type ImageRequestType string

const (
	RequestVMBaseImage ImageRequestType = "vm-base"
	RequestDockerImage ImageRequestType = "docker"
)

// ImageRequestResult holds information about a created request.
type ImageRequestResult struct {
	IssueNumber int
	IssueURL    string
}

// ImageRequester creates GitHub issues (or opens a browser) to request missing images.
type ImageRequester interface {
	// Request creates a request for a missing image.
	// When wait is true and a token is available, it polls until the asset appears.
	Request(ctx context.Context, reqType ImageRequestType, runtime, arch, tag, version string, wait bool) (*ImageRequestResult, error)
}
