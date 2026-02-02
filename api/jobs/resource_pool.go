package jobs

import (
	"sync"

	log "github.com/sirupsen/logrus"
)

// StatusResponse contains current resource utilization.
type StatusResponse struct {
	// Running job resources
	UsedCPUs   float32
	UsedMemory int
	// Queued job resources (waiting in PendingJobs)
	QueuedCPUs   float32
	QueuedMemory int
	// Maximum available resources
	MaxCPUs   float32
	MaxMemory int
}

// ResourcePool tracks available vs used resources for job scheduling.
// Uses mutex for thread-safe access to shared state.
type ResourcePool struct {
	mu sync.RWMutex

	maxCPUs   float32
	maxMemory int // in MB

	usedCPUs   float32
	usedMemory int

	queuedCPUs   float32
	queuedMemory int

	releaseNotify chan struct{} // Signals QueueWorker when resources are released
}

// NewResourcePool creates a ResourcePool with the given max limits.
// The limits should come from the centralized config to ensure consistency
// between resource pool and process validation.
func NewResourcePool(maxCPUs float32, maxMemory int) *ResourcePool {
	log.Infof("ResourcePool initialized: maxCPUs=%.2f, maxMemory=%dMB", maxCPUs, maxMemory)

	return &ResourcePool{
		maxCPUs:       maxCPUs,
		maxMemory:     maxMemory,
		releaseNotify: make(chan struct{}, 1),
	}
}

// TryReserve attempts to reserve resources for a running job.
// Returns true if successful, false if not enough resources available.
func (rp *ResourcePool) TryReserve(cpus float32, memory int) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	if rp.usedCPUs+cpus <= rp.maxCPUs && rp.usedMemory+memory <= rp.maxMemory {
		rp.usedCPUs += cpus
		rp.usedMemory += memory
		log.Debugf("Resources reserved: cpus=%.2f, memory=%dMB. Used: cpus=%.2f/%.2f, memory=%d/%dMB",
			cpus, memory, rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory)
		return true
	}
	return false
}

// ReserveForce increments used resources without enforcing limits.
// This is intended for recovery to reflect already-running jobs.
func (rp *ResourcePool) ReserveForce(cpus float32, memory int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.usedCPUs += cpus
	rp.usedMemory += memory
	log.Debugf("Resources forced: cpus=%.2f, memory=%dMB. Used: cpus=%.2f/%.2f, memory=%d/%dMB",
		cpus, memory, rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory)
}

// Release returns resources to the pool when a job finishes.
func (rp *ResourcePool) Release(cpus float32, memory int) {
	rp.mu.Lock()
	rp.usedCPUs -= cpus
	rp.usedMemory -= memory

	// Clamp to zero (safety check)
	if rp.usedCPUs < 0 {
		rp.usedCPUs = 0
	}
	if rp.usedMemory < 0 {
		rp.usedMemory = 0
	}

	log.Debugf("Resources released: cpus=%.2f, memory=%dMB. Used: cpus=%.2f/%.2f, memory=%d/%dMB",
		cpus, memory, rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory)
	rp.mu.Unlock()

	// Signal QueueWorker that resources are available
	select {
	case rp.releaseNotify <- struct{}{}:
	default:
	}
}

// AddQueued adds resources to the queued count when a job is enqueued to PendingJobs.
func (rp *ResourcePool) AddQueued(cpus float32, memory int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.queuedCPUs += cpus
	rp.queuedMemory += memory
	log.Debugf("Resources queued: cpus=%.2f, memory=%dMB. Queued: cpus=%.2f, memory=%dMB",
		cpus, memory, rp.queuedCPUs, rp.queuedMemory)
}

// RemoveQueued removes resources from the queued count when a job leaves PendingJobs.
func (rp *ResourcePool) RemoveQueued(cpus float32, memory int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.queuedCPUs -= cpus
	rp.queuedMemory -= memory

	// Clamp to zero (safety check)
	if rp.queuedCPUs < 0 {
		rp.queuedCPUs = 0
	}
	if rp.queuedMemory < 0 {
		rp.queuedMemory = 0
	}

	log.Debugf("Resources dequeued: cpus=%.2f, memory=%dMB. Queued: cpus=%.2f, memory=%dMB",
		cpus, memory, rp.queuedCPUs, rp.queuedMemory)
}

// GetStatus returns current resource utilization.
func (rp *ResourcePool) GetStatus() StatusResponse {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	return StatusResponse{
		UsedCPUs:     rp.usedCPUs,
		UsedMemory:   rp.usedMemory,
		QueuedCPUs:   rp.queuedCPUs,
		QueuedMemory: rp.queuedMemory,
		MaxCPUs:      rp.maxCPUs,
		MaxMemory:    rp.maxMemory,
	}
}

// ReleaseChan returns the channel that signals when resources are released.
func (rp *ResourcePool) ReleaseChan() <-chan struct{} {
	return rp.releaseNotify
}
