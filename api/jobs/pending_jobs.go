package jobs

import (
	"container/list"
	"sync"
)

// PendingJobs is a pure FIFO queue for jobs waiting to be executed.
// Only async Docker/Subprocess jobs that need local resource management go here.
//
// This is a pure data structure with no business logic - it just stores and
// retrieves jobs. Signaling and resource tracking are handled by QueueWorker
// and ResourcePool respectively.
//
// Uses a doubly-linked list + map for O(1) operations:
//   - list.List: maintains FIFO order, O(1) insert/remove at ends
//   - index map: jobID → list element pointer, O(1) lookup for Remove()
//
// Example:
//
//	list: job1 ◄──► job2 ◄──► job3
//	                 ▲
//	index: {"uuid-2" → pointer}
//
//	Remove("uuid-2"):
//	  1. Map lookup: O(1) to find element
//	  2. List remove: O(1) to update prev/next pointers
//	  Result: job1 ◄──► job3
type PendingJobs struct {
	list  *list.List
	index map[string]*list.Element
	mu    sync.Mutex
}

// NewPendingJobs creates a new PendingJobs queue.
func NewPendingJobs() *PendingJobs {
	return &PendingJobs{
		list:  list.New(),
		index: make(map[string]*list.Element),
	}
}

// Enqueue adds a job to the back of the queue.
func (pj *PendingJobs) Enqueue(j *Job) {
	pj.mu.Lock()
	defer pj.mu.Unlock()

	elem := pj.list.PushBack(j)
	pj.index[(*j).JobID()] = elem
}

// Peek returns the job at the front of the queue without removing it.
// Returns nil if the queue is empty.
func (pj *PendingJobs) Peek() *Job {
	pj.mu.Lock()
	defer pj.mu.Unlock()

	elem := pj.list.Front()
	if elem == nil {
		return nil
	}

	return elem.Value.(*Job)
}

// Remove removes a job by ID from anywhere in the queue.
// Returns the removed job, or nil if not found.
// O(1) lookup via map, O(1) removal from doubly-linked list.
func (pj *PendingJobs) Remove(jobID string) *Job {
	pj.mu.Lock()
	defer pj.mu.Unlock()

	elem, ok := pj.index[jobID]
	if !ok {
		return nil
	}

	delete(pj.index, jobID)
	return pj.list.Remove(elem).(*Job)
}

// Len returns the number of jobs in the queue.
func (pj *PendingJobs) Len() int {
	pj.mu.Lock()
	defer pj.mu.Unlock()
	return pj.list.Len()
}
