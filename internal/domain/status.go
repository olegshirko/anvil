package domain

// StatusInfo represents the status of the application.
type StatusInfo struct {
	DisplayName      string `json:"display_name"`
	Driver           string `json:"driver"`
	Arch             string `json:"arch"`
	Runtime          string `json:"runtime"`
	MountType        string `json:"mount_type"`
	IPAddress        string `json:"ip_address,omitempty"`
	DockerSocket     string `json:"docker_socket,omitempty"`
	ContainerdSocket string `json:"containerd_socket,omitempty"`
	BuildkitdSocket  string `json:"buildkitd_socket,omitempty"`
	IncusSocket      string `json:"incus_socket,omitempty"`
	Kubernetes       bool   `json:"kubernetes"`
	CPU              int    `json:"cpu"`
	Memory           int64  `json:"memory"`
	Disk             int64  `json:"disk"`
}

// HealthReport represents the health status of a profile.
type HealthReport struct {
	Profile string        `json:"profile"`
	Overall string        `json:"overall"` // healthy, degraded, unhealthy
	Checks  []HealthCheck `json:"checks"`
}

// HealthCheck represents a single health check result.
type HealthCheck struct {
	Component string `json:"component"`
	Status    string `json:"status"` // ok, warning, error
	Message   string `json:"message,omitempty"`
}

// Diagnosis represents a single doctor check result.
type Diagnosis struct {
	Check   string `json:"check"`
	Status  string `json:"status"` // ok, warn, fail
	Message string `json:"message,omitempty"`
	Fix     string `json:"fix,omitempty"`
}
