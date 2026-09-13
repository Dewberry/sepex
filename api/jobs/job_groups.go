package jobs

import "time"

// A job group is a set of ordinary jobs submitted in one request and tracked as
// one unit. The group is only a submission and tracking mechanism: every member
// is an ordinary SEPEX job, with its own status, logs, results and metadata at
// /jobs/{jobID}, and it runs exactly as it would have if submitted on its own.
//
// A group holds no in-memory state. Everything it knows is in the database, so
// a restart has nothing to rebuild for the group itself and recovery concerns
// only its members.

// GroupTagPrefix marks the tag SEPEX writes onto every member of a group, so
// that a group's jobs can be found through the ordinary /jobs tag filter. The
// prefix is reserved at submission, so a client cannot claim membership by
// tagging an unrelated job.
const GroupTagPrefix = "group:"

// StatusNotCreated selects the members whose job could not be created. It is
// accepted by the member status filter on a group and is the one value there
// that is not a job status, because such a member has no job to carry one.
const StatusNotCreated = "notCreated"

// GroupTag returns the tag written onto every member of a group. A group ID is
// a UUID, so this tag has a fixed length and contains none of the characters
// that make tag matching on /jobs ambiguous: no two group tags can prefix-match
// each other, and none contains a LIKE wildcard.
func GroupTag(groupID string) string {
	return GroupTagPrefix + groupID
}

// JobGroupRecord contains details about a group.
type JobGroupRecord struct {
	GroupID   string
	Submitter string
	Tags      []string

	// Requested is how many members the submission asked for. It is fixed when
	// the group is created and never changes, so it stays the honest count of
	// what was asked for however submission turns out.
	Requested int

	// Message carries why a submission did not create everything it was asked
	// to, and is empty otherwise.
	Message string

	Created time.Time

	// Submitted is nil while members are still being created. Submission runs
	// in the background, so this is what separates "more members are still
	// arriving" from "nothing further is coming", whether submission reached
	// the end of the list or stopped early.
	Submitted *time.Time

	// Dismissed records when DELETE was called on the group. SEPEX never
	// dismisses a group's members on its own, so this is only ever set by a
	// request.
	Dismissed *time.Time
}

// JobGroupMember is one position in a group.
//
// A member whose job could not be created keeps its position and carries the
// reason in place of a job. That is what lets a client see which entries of its
// request need resubmitting, rather than inferring them from gaps.
type JobGroupMember struct {
	Position int `json:"position"`

	// JobID is empty when the job could not be created; everything below it
	// comes from the job and is empty for such a member.
	JobID      string     `json:"jobID,omitempty"`
	ProcessID  string     `json:"processID,omitempty"`
	Status     string     `json:"status,omitempty"`
	LastUpdate *time.Time `json:"updated,omitempty"`
	Tags       []string   `json:"tags"`

	// Error is why this member's job could not be created, empty otherwise.
	Error string `json:"error,omitempty"`
}

// JobGroupSummary counts a group's members by status.
type JobGroupSummary struct {
	Accepted   int `json:"accepted"`
	Running    int `json:"running"`
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
	Dismissed  int `json:"dismissed"`
	Lost       int `json:"lost"`

	// NotCreated counts members that have no job at all. It is kept separate
	// from Failed because the two say different things to whoever is reading:
	// a failed member ran and did not succeed, while one of these never
	// existed and its work was never attempted.
	NotCreated int `json:"notCreated"`
}

// Created counts the members whose job exists, whatever state it is in.
func (s JobGroupSummary) Created() int {
	return s.Accepted + s.Running + s.Successful + s.Failed + s.Dismissed + s.Lost
}

// FillNotCreated works out how many members have no job, from how many were
// asked for and how many exist. Counting it this way covers both members whose
// creation was attempted and failed and members that were never attempted at
// all, because neither has a job.
func (s *JobGroupSummary) FillNotCreated(requested int) {
	if n := requested - s.Created(); n > 0 {
		s.NotCreated = n
		return
	}
	s.NotCreated = 0
}

// addStatus counts n members reported in this status. A status the server does
// not know is ignored rather than guessed at, so a summary never claims more
// members than it can account for.
func (s *JobGroupSummary) addStatus(status string, n int) {
	switch status {
	case ACCEPTED:
		s.Accepted += n
	case RUNNING:
		s.Running += n
	case SUCCESSFUL:
		s.Successful += n
	case FAILED:
		s.Failed += n
	case DISMISSED:
		s.Dismissed += n
	case LOST:
		s.Lost += n
	}
}

// splitMemberStatusFilter separates a member status filter into job statuses
// and whether members without a job were asked for. The two are matched
// differently: a job status is a property of the job, while "not created" is
// the absence of one.
func splitMemberStatusFilter(statuses []string) (jobStatuses []string, includeNotCreated bool) {
	for _, s := range statuses {
		if s == StatusNotCreated {
			includeNotCreated = true
			continue
		}
		jobStatuses = append(jobStatuses, s)
	}
	return jobStatuses, includeNotCreated
}

// GroupStatus reduces a group's members to one status for the group.
//
// The group is never reported as finished while members are still being
// created, however the ones created so far have turned out. Otherwise a group
// whose first few members happened to succeed would report successful while the
// rest of the work was still being submitted.
//
// Once nothing further is coming, a group is only as good as its worst member:
//
//   - failed, if any member failed, was lost, or was never created. Members
//     that could not be created count here because the work they were asked to
//     do does not exist, which is a failure of the group however well the
//     others ran. There is no fail-fast: the group reports failed once its
//     members are all finished, not at the first failure.
//   - dismissed, if nothing failed but a member was dismissed. Recovery
//     dismisses local members that were still queued when the server stopped,
//     so a group can reach this without anyone having called DELETE.
//   - successful, only when every member the group asked for succeeded.
func GroupStatus(rec JobGroupRecord, s JobGroupSummary) string {
	inFlight := s.Accepted + s.Running

	if rec.Submitted == nil || inFlight > 0 {
		// Nothing has started when every member that exists is still queued,
		// which includes a group whose members are all still to be created.
		if s.Running == 0 && s.Accepted == s.Created() {
			return ACCEPTED
		}
		return RUNNING
	}

	switch {
	case s.Failed+s.Lost+s.NotCreated > 0:
		return FAILED
	case s.Dismissed > 0:
		return DISMISSED
	default:
		return SUCCESSFUL
	}
}
