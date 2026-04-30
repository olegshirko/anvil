package domain

// Instance represents a running instance of the VM.
type Instance struct {
	CPU    int
	Memory int64
	Disk   int64
}
