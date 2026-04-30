package domain

// InstanceInfo represents information about a VM instance for listing.
type InstanceInfo struct {
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	Arch      string `json:"arch,omitempty"`
	CPU       int    `json:"cpus,omitempty"`
	Memory    int64  `json:"memory,omitempty"`
	Disk      int64  `json:"disk,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
	IPAddress string `json:"address,omitempty"`
	Dir       string `json:"dir,omitempty"`
}
