// Package container provides primitives for running and
// managing containers on this machine. Under the hood, it
// uses containerd running on this machine.
package container

import (
	"sync"
)

// Status represents the status of a container.
type Status int

// Statuses of a container.
const (
	Unknown Status = iota
	Stopped
	Starting
	Running
	Stopping
)

// Container represents a container running on this machine.
type Container struct {
	ID       string
	ImageURI string

	shutdown chan struct{}

	mu     sync.RWMutex // Protects everything below.
	status Status
}

// Status returns the status of the container.
func (c *Container) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}
