package jobs

import (
	"sync"
	"testing"
	"time"
)

func TestAWSBatchJobRunFinishedOnceUnderConcurrentCompletion(t *testing.T) {
	const jobCount = 20
	const completionsPerJob = 10

	for range jobCount {
		j := &AWSBatchJob{}
		j.wgRun.Add(1)

		var callers sync.WaitGroup
		callers.Add(completionsPerJob)
		for range completionsPerJob {
			go func() {
				defer callers.Done()
				j.RunFinished()
			}()
		}
		callers.Wait()

		done := make(chan struct{})
		go func() {
			j.WaitForRunCompletion()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("job completion did not unblock waiters")
		}
	}
}
