package jobs

import (
	"sync"

	log "github.com/sirupsen/logrus"
)

// QueueWorker is the scheduler that starts pending jobs when resources are available.
//
// Responsibilities:
//   - Listens for signals indicating work may be available
//   - Attempts to start jobs from PendingJobs queue
//   - Coordinates with ResourcePool for resource reservation
//   - Moves resources from "queued" to "used" when jobs start
//
// Event-driven: wakes on new job signal or resource release signal.
type QueueWorker struct {
	pendingJobs  *PendingJobs
	resourcePool *ResourcePool
	workSignal   chan struct{} // Signals that new work may be available
	shutdown     chan struct{}
	wg           sync.WaitGroup
}

// NewQueueWorker creates a new QueueWorker.
func NewQueueWorker(pendingJobs *PendingJobs, resourcePool *ResourcePool) *QueueWorker {
	return &QueueWorker{
		pendingJobs:  pendingJobs,
		resourcePool: resourcePool,
		workSignal:   make(chan struct{}, 1),
		shutdown:     make(chan struct{}),
	}
}

// Start begins the queue processing goroutine.
func (qw *QueueWorker) Start() {
	qw.wg.Add(1)
	go qw.processLoop()
	log.Info("QueueWorker started")
}

// Stop signals the queue worker to shutdown and waits for it to finish.
func (qw *QueueWorker) Stop() {
	close(qw.shutdown)
	qw.wg.Wait()
	log.Info("QueueWorker stopped")
}

// NotifyNewJob signals that a new job has been enqueued.
// Called by Handler after adding a job to PendingJobs.
func (qw *QueueWorker) NotifyNewJob() {
	select {
	case qw.workSignal <- struct{}{}:
	default:
		// Channel already has a pending signal; worker will process all jobs when it wakes
	}
}

// processLoop waits for signals and processes pending jobs.
func (qw *QueueWorker) processLoop() {
	defer qw.wg.Done()

	for {
		select {
		case <-qw.shutdown:
			log.Info("QueueWorker shutting down")
			return
		case <-qw.workSignal:
			qw.tryStartJobs()
		case <-qw.resourcePool.ReleaseChan():
			qw.tryStartJobs()
		}
	}
}

// tryStartJobs processes pending jobs until queue is empty or resources unavailable.
func (qw *QueueWorker) tryStartJobs() {
	for {
		job := qw.pendingJobs.Peek()
		if job == nil {
			return
		}

		res := (*job).GetResources()
		if !qw.resourcePool.TryReserve(res.CPUs, res.Memory) {
			return // Not enough resources, wait for release
		}

		// Remove the same job we peeked; it may have been dismissed concurrently, so can't use dequeue directly.
		removed := qw.pendingJobs.Remove((*job).JobID())
		if removed == nil {
			// Job disappeared between peek and remove; release reservation and retry.
			qw.resourcePool.Release(res.CPUs, res.Memory)
			continue
		}

		// Job is leaving the queue and starting - update resource tracking.
		// Resources removed from "queued" (TryReserve already added to "used").
		qw.resourcePool.RemoveQueued(res.CPUs, res.Memory)

		log.Infof("Starting job %s", (*removed).JobID())
		go (*removed).Run()
	}
}
