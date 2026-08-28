package jobs

import (
	"sort"
	"strconv"
	"sync"

	log "github.com/sirupsen/logrus"
)

// GPUDevice identifies a single GPU that the pool can allocate.
//
// Unlike CPUs and memory, which are advisory inputs to scheduling only, a GPU
// is allocated as a whole, exclusive unit and the allocation is enforced at
// container launch. The pool therefore tracks device identity rather than a
// count: it must be able to name which device a job was given.
type GPUDevice struct {
	Index int
	// UUID is empty only when GPU verification was skipped, since nothing
	// then enumerated the hardware.
	UUID string
}

// DeviceID returns the identifier to pass to Docker in a DeviceRequest.
// Docker accepts either form. A UUID is preferred because it is stable across
// reboots and driver reordering, but an index is all that is available when
// verification is skipped.
func (d GPUDevice) DeviceID() string {
	if d.UUID != "" {
		return d.UUID
	}
	return strconv.Itoa(d.Index)
}

// GPUStatus reports one device and, when allocated, the job holding it.
type GPUStatus struct {
	Index int
	UUID  string
	JobID string // empty when the device is free
}

// StatusResponse contains current resource utilization.
type StatusResponse struct {
	// Running job resources
	UsedCPUs   float32
	UsedMemory int
	UsedGPUs   int
	// Queued job resources (waiting in PendingJobs)
	QueuedCPUs   float32
	QueuedMemory int
	QueuedGPUs   int
	// Maximum available resources
	MaxCPUs   float32
	MaxMemory int
	MaxGPUs   int
	// GPUs lists every device in index order, allocated and free alike.
	GPUs []GPUStatus
}

// ResourcePool tracks available vs used resources for job scheduling.
// Uses mutex for thread-safe access to shared state.
//
// CPUs and memory are counters: they are only advisory hints about how many
// jobs may run at once, and nothing enforces them at launch. GPUs are the
// exception. Because a GPU cannot be subdivided or shared, the pool hands out
// specific devices and must know exactly which job holds which one.
type ResourcePool struct {
	mu sync.RWMutex

	maxCPUs   float32
	maxMemory int // in MB

	usedCPUs   float32
	usedMemory int

	// gpus is the fixed set of devices this pool may allocate, in index order.
	gpus []GPUDevice
	// gpuHolders maps a device index to the job holding it. A device absent
	// from this map is free, which makes a duplicate release a no-op rather
	// than something that could hand the same device to two jobs.
	gpuHolders map[int]string

	queuedCPUs   float32
	queuedMemory int
	queuedGPUs   int

	releaseNotify chan struct{} // Signals QueueWorker when resources are released
}

// NewResourcePool creates a ResourcePool with the given max limits.
// The limits should come from the centralized config to ensure consistency
// between resource pool and process validation.
//
// gpus is the exact set of devices this instance may schedule onto. It is
// copied, so later mutation by the caller cannot corrupt the pool.
func NewResourcePool(maxCPUs float32, maxMemory int, gpus []GPUDevice) *ResourcePool {
	devices := make([]GPUDevice, len(gpus))
	copy(devices, gpus)
	sort.Slice(devices, func(i, j int) bool { return devices[i].Index < devices[j].Index })

	log.Infof("ResourcePool initialized: maxCPUs=%.2f, maxMemory=%dMB, maxGPUs=%d", maxCPUs, maxMemory, len(devices))

	return &ResourcePool{
		maxCPUs:       maxCPUs,
		maxMemory:     maxMemory,
		gpus:          devices,
		gpuHolders:    make(map[int]string, len(devices)),
		releaseNotify: make(chan struct{}, 1),
	}
}

// selectFreeGPUsLocked returns the n lowest-numbered free devices, or false if
// fewer than n are available. Caller must hold the lock.
func (rp *ResourcePool) selectFreeGPUsLocked(n int) ([]GPUDevice, bool) {
	if n <= 0 {
		return nil, true
	}

	selected := make([]GPUDevice, 0, n)
	for _, d := range rp.gpus {
		if _, held := rp.gpuHolders[d.Index]; held {
			continue
		}
		selected = append(selected, d)
		if len(selected) == n {
			return selected, true
		}
	}
	return nil, false
}

// TryReserve attempts to reserve resources for a running job.
// Returns the GPU devices assigned to the job (nil when none were requested)
// and whether the reservation succeeded. Nothing is reserved on failure.
func (rp *ResourcePool) TryReserve(jobID string, cpus float32, memory int, gpus int) ([]GPUDevice, bool) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	if rp.usedCPUs+cpus > rp.maxCPUs || rp.usedMemory+memory > rp.maxMemory {
		return nil, false
	}

	assigned, ok := rp.selectFreeGPUsLocked(gpus)
	if !ok {
		return nil, false
	}

	rp.usedCPUs += cpus
	rp.usedMemory += memory
	for _, d := range assigned {
		rp.gpuHolders[d.Index] = jobID
	}

	log.Debugf("Resources reserved: cpus=%.2f, memory=%dMB, gpus=%v. Used: cpus=%.2f/%.2f, memory=%d/%dMB, gpus=%d/%d",
		cpus, memory, gpuIndices(assigned), rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory, len(rp.gpuHolders), len(rp.gpus))
	return assigned, true
}

// ReserveForce increments used resources without enforcing limits.
// This is intended for recovery to reflect already-running jobs.
//
// Unlike TryReserve, the devices are dictated by the caller: a surviving
// container already holds specific GPUs, so the pool must reclaim exactly
// those rather than picking its own.
func (rp *ResourcePool) ReserveForce(jobID string, cpus float32, memory int, devices []GPUDevice) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.usedCPUs += cpus
	rp.usedMemory += memory

	for _, d := range devices {
		if !rp.knownGPULocked(d.Index) {
			log.Warnf("Resource reclaim: GPU %d is not in this pool and cannot be reclaimed for job=%s", d.Index, jobID)
			continue
		}
		if holder, held := rp.gpuHolders[d.Index]; held {
			log.Warnf("Resource reclaim: GPU %d already held by job=%s, not reassigning to job=%s", d.Index, holder, jobID)
			continue
		}
		rp.gpuHolders[d.Index] = jobID
	}

	log.Debugf("Resources forced: cpus=%.2f, memory=%dMB, gpus=%v. Used: cpus=%.2f/%.2f, memory=%d/%dMB, gpus=%d/%d",
		cpus, memory, gpuIndices(devices), rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory, len(rp.gpuHolders), len(rp.gpus))
}

// knownGPULocked reports whether the device index belongs to this pool.
// Caller must hold the lock.
func (rp *ResourcePool) knownGPULocked(index int) bool {
	for _, d := range rp.gpus {
		if d.Index == index {
			return true
		}
	}
	return false
}

// Release returns resources to the pool when a job finishes.
// devices must be the ones handed out by TryReserve or ReserveForce; passing
// a device that is not currently allocated is logged and ignored.
func (rp *ResourcePool) Release(cpus float32, memory int, devices []GPUDevice) {
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

	for _, d := range devices {
		if _, held := rp.gpuHolders[d.Index]; !held {
			log.Warnf("Resources released: GPU %d was not allocated, ignoring", d.Index)
			continue
		}
		delete(rp.gpuHolders, d.Index)
	}

	log.Debugf("Resources released: cpus=%.2f, memory=%dMB, gpus=%v. Used: cpus=%.2f/%.2f, memory=%d/%dMB, gpus=%d/%d",
		cpus, memory, gpuIndices(devices), rp.usedCPUs, rp.maxCPUs, rp.usedMemory, rp.maxMemory, len(rp.gpuHolders), len(rp.gpus))
	rp.mu.Unlock()

	// Signal QueueWorker that resources are available
	select {
	case rp.releaseNotify <- struct{}{}:
	default:
	}
}

// AddQueued adds resources to the queued count when a job is enqueued to PendingJobs.
func (rp *ResourcePool) AddQueued(cpus float32, memory int, gpus int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.queuedCPUs += cpus
	rp.queuedMemory += memory
	rp.queuedGPUs += gpus
	log.Debugf("Resources queued: cpus=%.2f, memory=%dMB, gpus=%d. Queued: cpus=%.2f, memory=%dMB, gpus=%d",
		cpus, memory, gpus, rp.queuedCPUs, rp.queuedMemory, rp.queuedGPUs)
}

// RemoveQueued removes resources from the queued count when a job leaves PendingJobs.
func (rp *ResourcePool) RemoveQueued(cpus float32, memory int, gpus int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	rp.queuedCPUs -= cpus
	rp.queuedMemory -= memory
	rp.queuedGPUs -= gpus

	// Clamp to zero (safety check)
	if rp.queuedCPUs < 0 {
		rp.queuedCPUs = 0
	}
	if rp.queuedMemory < 0 {
		rp.queuedMemory = 0
	}
	if rp.queuedGPUs < 0 {
		rp.queuedGPUs = 0
	}

	log.Debugf("Resources dequeued: cpus=%.2f, memory=%dMB, gpus=%d. Queued: cpus=%.2f, memory=%dMB, gpus=%d",
		cpus, memory, gpus, rp.queuedCPUs, rp.queuedMemory, rp.queuedGPUs)
}

// GetStatus returns current resource utilization.
func (rp *ResourcePool) GetStatus() StatusResponse {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	gpus := make([]GPUStatus, len(rp.gpus))
	for i, d := range rp.gpus {
		gpus[i] = GPUStatus{Index: d.Index, UUID: d.UUID, JobID: rp.gpuHolders[d.Index]}
	}

	return StatusResponse{
		UsedCPUs:     rp.usedCPUs,
		UsedMemory:   rp.usedMemory,
		UsedGPUs:     len(rp.gpuHolders),
		QueuedCPUs:   rp.queuedCPUs,
		QueuedMemory: rp.queuedMemory,
		QueuedGPUs:   rp.queuedGPUs,
		MaxCPUs:      rp.maxCPUs,
		MaxMemory:    rp.maxMemory,
		MaxGPUs:      len(rp.gpus),
		GPUs:         gpus,
	}
}

// ReleaseChan returns the channel that signals when resources are released.
func (rp *ResourcePool) ReleaseChan() <-chan struct{} {
	return rp.releaseNotify
}

// gpuIndices renders devices for logging.
func gpuIndices(devices []GPUDevice) []int {
	indices := make([]int, len(devices))
	for i, d := range devices {
		indices[i] = d.Index
	}
	return indices
}
