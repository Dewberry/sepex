package handlers

import (
	"app/jobs"
	pr "app/processes"
	"app/utils"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Group submission runs in the background. The request that creates a group
// returns as soon as the group itself is recorded, because creating its members
// can take far longer than a client should hold a connection open: on aws-batch
// every member costs a SubmitJob round trip, so a large group takes minutes.
//
// Everything a client could have been told by a failed request is therefore
// reported on the group instead, and the request only promises that the group
// was accepted, not that its members exist yet.

const (
	// maxConsecutiveCreateFailures stops a submission that is failing for a
	// reason no further member will avoid: the database being down, or Batch
	// refusing every submission. Isolated failures are survivable and do not
	// stop the rest of the group, which is the common case worth protecting,
	// since members differ only by their inputs and one oversized payload
	// should not sink the other members.
	maxConsecutiveCreateFailures = 10

	// maxConcurrentGroupSubmissions bounds how many groups are being submitted
	// at once, so that several large groups cannot collectively saturate the
	// account wide AWS Batch submission rate that every other job shares. A
	// group waiting for a slot simply stays in submission for longer, which its
	// status already says.
	maxConcurrentGroupSubmissions = 4
)

// GroupSubmitter creates the members of accepted job groups in the background.
type GroupSubmitter struct {
	rh *RESTHandler

	mu       sync.Mutex
	inFlight map[string]*groupSubmission
	stopped  bool

	slots chan struct{}
	wg    sync.WaitGroup
}

// groupSubmission is one submission in flight. The reason is set before the
// cancel is called, so that the submission can report why it stopped rather
// than only that it did.
type groupSubmission struct {
	cancel context.CancelFunc
	reason string
}

func NewGroupSubmitter(rh *RESTHandler) *GroupSubmitter {
	return &GroupSubmitter{
		rh:       rh,
		inFlight: make(map[string]*groupSubmission),
		slots:    make(chan struct{}, maxConcurrentGroupSubmissions),
	}
}

// Submit starts creating a group's members and returns immediately. The group
// record must already be stored, so that a client polling it straight away
// finds it.
func (gs *GroupSubmitter) Submit(rec jobs.JobGroupRecord, p pr.Process, entries []groupJobRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	sub := &groupSubmission{cancel: cancel}

	gs.mu.Lock()
	if gs.stopped {
		gs.mu.Unlock()
		cancel()
		gs.finish(rec, 0, 0, len(entries), "the server was shutting down when the group was accepted", "")
		return
	}
	gs.inFlight[rec.GroupID] = sub
	gs.wg.Add(1)
	gs.mu.Unlock()

	go func() {
		defer gs.wg.Done()
		defer cancel()
		defer func() {
			gs.mu.Lock()
			delete(gs.inFlight, rec.GroupID)
			gs.mu.Unlock()
		}()

		select {
		case gs.slots <- struct{}{}:
			defer func() { <-gs.slots }()
		case <-ctx.Done():
			gs.finish(rec, 0, 0, len(entries), gs.reasonFor(rec.GroupID), "")
			return
		}

		gs.run(ctx, rec, p, entries)
	}()
}

// Cancel stops a group's submission if one is still running, so that no member
// is created behind a dismissal.
func (gs *GroupSubmitter) Cancel(groupID, reason string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	if sub, ok := gs.inFlight[groupID]; ok {
		sub.reason = reason
		sub.cancel()
	}
}

// Stop cancels every submission in flight and waits for them to finish, so that
// none is still writing when the database closes. Submissions check for
// cancellation between members, so this returns as soon as the members being
// created finish.
func (gs *GroupSubmitter) Stop(timeout time.Duration) {
	gs.mu.Lock()
	gs.stopped = true
	for _, sub := range gs.inFlight {
		sub.reason = "the server was shut down"
		sub.cancel()
	}
	inFlight := len(gs.inFlight)
	gs.mu.Unlock()

	if inFlight == 0 {
		return
	}
	log.Infof("Waiting for %d job group submission(s) to stop", inFlight)

	done := make(chan struct{})
	go func() {
		gs.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info("job group submissions stopped")
	case <-time.After(timeout):
		log.Warn("job group submissions did not stop in time; their groups will be closed out at the next startup")
	}
}

// reasonFor reports why a submission was stopped.
func (gs *GroupSubmitter) reasonFor(groupID string) string {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	if sub, ok := gs.inFlight[groupID]; ok && sub.reason != "" {
		return sub.reason
	}
	return "submission was stopped"
}

// run creates the members in submission order, one at a time.
//
// A member that cannot be created does not stop the ones after it. Its position
// keeps a row carrying the reason, and the group reports how much of what was
// asked for exists. Killing the members that did work because a later one
// failed would throw away good work, so nothing already created is touched.
func (gs *GroupSubmitter) run(ctx context.Context, rec jobs.JobGroupRecord, p pr.Process, entries []groupJobRequest) {
	created, consecutive := 0, 0
	firstError := ""

	for i, entry := range entries {
		if ctx.Err() != nil {
			gs.finish(rec, created, i, len(entries), gs.reasonFor(rec.GroupID), firstError)
			return
		}

		err := gs.createMember(rec, p, i, entry)
		if err == nil {
			created++
			consecutive = 0
			continue
		}

		consecutive++
		if firstError == "" {
			firstError = err.Error()
		}
		log.Errorf("group %s: member %d could not be created: %s", rec.GroupID, i, err.Error())

		if dbErr := gs.rh.DB.AddJobGroupMember(rec.GroupID, i, "", err.Error()); dbErr != nil {
			log.Errorf("group %s: could not record the failure of member %d: %s", rec.GroupID, i, dbErr.Error())
		}

		if consecutive >= maxConsecutiveCreateFailures {
			gs.finish(rec, created, i+1, len(entries),
				fmt.Sprintf("submission stopped after %d consecutive failures", consecutive), firstError)
			return
		}
	}

	gs.finish(rec, created, len(entries), len(entries), "", firstError)
}

// createMember creates one member and hands it to the queue, exactly as a job
// submitted on its own would be.
func (gs *GroupSubmitter) createMember(rec jobs.JobGroupRecord, p pr.Process, position int, entry groupJobRequest) error {
	cmd, err := buildCommand(p, entry.Inputs)
	if err != nil {
		return err
	}

	jobID := uuid.New().String()
	j := gs.rh.buildJob(jobID, p, cmd, memberTags(rec, entry.Tags), rec.Submitter, false)
	if j == nil {
		return fmt.Errorf("unsupported host type %s", p.Host.Type)
	}

	if err := j.Create(); err != nil {
		return err
	}

	// Recorded before the job is handed to the queue, so that a dismissal
	// arriving moments later finds it. A member whose row cannot be written is
	// left running rather than killed: it is an ordinary job, it carries the
	// group tag, and it is reachable at /jobs like any other.
	if err := gs.rh.DB.AddJobGroupMember(rec.GroupID, position, jobID, ""); err != nil {
		log.Errorf("group %s: member %d (job %s) was created but could not be recorded, so the group will not list it: %s",
			rec.GroupID, position, jobID, err.Error())
	}

	gs.rh.ActiveJobs.Add(&j)

	// Only docker and subprocess jobs wait for local resources. An aws-batch
	// job was already submitted by Create().
	switch j.(type) {
	case *jobs.DockerJob, *jobs.SubprocessJob:
		res := j.GetResources()
		gs.rh.ResourcePool.AddQueued(res.CPUs, res.Memory, res.GPUs)
		gs.rh.PendingJobs.Enqueue(&j)
		gs.rh.QueueWorker.NotifyNewJob()
	}

	return nil
}

// finish closes out a submission.
//
// The group is stamped as submitted whatever happened, including when
// submission stopped early: the stamp only says that no further member is
// coming. How complete the group is is answered by counting the members that
// exist against the number requested.
func (gs *GroupSubmitter) finish(rec jobs.JobGroupRecord, created, attempted, total int, stopReason, firstError string) {
	// Give the positions that were never reached a row too, so that every
	// position the group asked for can be seen and resubmitted. This is best
	// effort: the database is the likeliest reason a submission stopped early,
	// and the group is still correct without these rows, since a member with no
	// row at all is counted as not created just the same.
	if stopReason != "" {
		for i := attempted; i < total; i++ {
			if err := gs.rh.DB.AddJobGroupMember(rec.GroupID, i, "", "not attempted: "+stopReason); err != nil {
				log.Warnf("group %s: could not record member %d as not attempted: %s", rec.GroupID, i, err.Error())
				break
			}
		}
	}

	message := ""
	if missing := total - created; missing > 0 {
		message = fmt.Sprintf("%d of %d members could not be created", missing, total)
		if stopReason != "" {
			message += "; " + stopReason
		}
		if firstError != "" {
			message += ". First error: " + firstError
		}
	}

	if err := gs.rh.DB.UpdateJobGroupSubmitted(rec.GroupID, time.Now(), message); err != nil {
		log.Errorf("group %s: could not record the end of submission: %s", rec.GroupID, err.Error())
		return
	}

	log.Infof("group %s: submission finished, %d of %d members created", rec.GroupID, created, total)
}

// memberTags are the tags written onto one member: the group tag that makes a
// group's jobs findable through the ordinary /jobs tag filter, the group's own
// tags, and the member's.
func memberTags(rec jobs.JobGroupRecord, entryTags []string) []string {
	tags := make([]string, 0, len(rec.Tags)+len(entryTags)+1)
	tags = append(tags, jobs.GroupTag(rec.GroupID))

	for _, t := range append(append([]string{}, rec.Tags...), entryTags...) {
		if !utils.StringInSlice(t, tags) {
			tags = append(tags, t)
		}
	}
	return tags
}

// FinalizeInterruptedGroups closes out groups whose submission was still
// running when the server stopped, which is the only way a group can be left
// waiting for members that will never arrive.
//
// The members already created are left alone. They are ordinary jobs, they
// recover under the ordinary rules, and SEPEX does not dismiss a group's
// members of its own accord.
func (rh *RESTHandler) FinalizeInterruptedGroups() error {
	groups, err := rh.DB.GetSubmittingJobGroups()
	if err != nil {
		return err
	}

	for _, rec := range groups {
		summary, _, err := rh.DB.GetJobGroupSummary(rec.GroupID)
		if err != nil {
			log.Errorf("Recovery(group): could not summarize group %s: %s", rec.GroupID, err.Error())
			continue
		}

		created := summary.Created()
		message := ""
		if missing := rec.Requested - created; missing > 0 {
			message = fmt.Sprintf("%d of %d members could not be created; submission was interrupted by a service restart",
				missing, rec.Requested)
		}

		if err := rh.DB.UpdateJobGroupSubmitted(rec.GroupID, time.Now(), message); err != nil {
			log.Errorf("Recovery(group): could not close out group %s: %s", rec.GroupID, err.Error())
			continue
		}

		log.Infof("Recovery(group): group %s submission was interrupted; %d of %d members were created",
			rec.GroupID, created, rec.Requested)
	}

	return nil
}
