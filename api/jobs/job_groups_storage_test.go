package jobs

import (
	"path/filepath"
	"testing"
	"time"
)

// These run against a real SQLite database created by the same code the server
// uses, so the schema and every group query are exercised rather than mocked.

func newTestDB(t *testing.T) *SQLiteDB {
	t.Helper()

	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("could not open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

// addTestJob adds a job the way a job type does when it is created.
func addTestJob(t *testing.T, db *SQLiteDB, jobID, status string) {
	t.Helper()

	if err := db.addJob(jobID, status, "", "docker", "", "pyecho", "a@b.c", []string{"q:400"}, time.Now()); err != nil {
		t.Fatalf("could not add job %s: %v", jobID, err)
	}
}

// newTestGroup stores a group of four requested members: two with jobs, one
// whose job could not be created, and one that was never attempted.
func newTestGroup(t *testing.T, db *SQLiteDB) JobGroupRecord {
	t.Helper()

	rec := JobGroupRecord{
		GroupID:   "group-1",
		Submitter: "a@b.c",
		Tags:      []string{"reach:123"},
		Requested: 4,
		Created:   time.Now(),
	}
	if err := db.AddJobGroup(rec); err != nil {
		t.Fatalf("could not add group: %v", err)
	}

	addTestJob(t, db, "job-0", SUCCESSFUL)
	addTestJob(t, db, "job-1", RUNNING)

	for i, jobID := range []string{"job-0", "job-1"} {
		if err := db.AddJobGroupMember(rec.GroupID, i, jobID, ""); err != nil {
			t.Fatalf("could not add member %d: %v", i, err)
		}
	}
	if err := db.AddJobGroupMember(rec.GroupID, 2, "", "aws batch refused the submission"); err != nil {
		t.Fatalf("could not add failed member: %v", err)
	}

	return rec
}

func TestJobGroupRoundTrip(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	got, ok, err := db.GetJobGroup(rec.GroupID)
	if err != nil || !ok {
		t.Fatalf("GetJobGroup() ok = %v, err = %v", ok, err)
	}

	if got.Submitter != rec.Submitter || got.Requested != rec.Requested {
		t.Errorf("got %+v, want submitter %q and %d requested", got, rec.Submitter, rec.Requested)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "reach:123" {
		t.Errorf("tags = %v, want [reach:123]", got.Tags)
	}
	// Nothing has finished submitting it, so it must read as still submitting.
	if got.Submitted != nil || got.Dismissed != nil {
		t.Errorf("submitted = %v, dismissed = %v, want both unset", got.Submitted, got.Dismissed)
	}

	if _, ok, err := db.GetJobGroup("no-such-group"); ok || err != nil {
		t.Errorf("GetJobGroup(missing) ok = %v, err = %v, want false and no error", ok, err)
	}
}

func TestJobGroupSummaryCountsMembersWithoutJobs(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	summary, latest, err := db.GetJobGroupSummary(rec.GroupID)
	if err != nil {
		t.Fatalf("GetJobGroupSummary() error: %v", err)
	}
	summary.FillNotCreated(rec.Requested)

	if summary.Successful != 1 || summary.Running != 1 {
		t.Errorf("summary = %+v, want one successful and one running", summary)
	}
	// One member failed to be created and one was never attempted. Neither has
	// a job, so both are counted the same way.
	if summary.NotCreated != 2 {
		t.Errorf("notCreated = %d, want 2", summary.NotCreated)
	}
	if summary.Created() != 2 {
		t.Errorf("created = %d, want 2", summary.Created())
	}
	if latest.IsZero() {
		t.Error("latest member update is zero, want the most recent job update")
	}

	// A group still being submitted is never terminal, even though every
	// member that exists has finished.
	if status := GroupStatus(rec, summary); status != RUNNING {
		t.Errorf("GroupStatus() = %q, want %q while submitting", status, RUNNING)
	}
}

func TestJobGroupMembers(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	members, err := db.GetJobGroupMembers(rec.GroupID, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetJobGroupMembers() error: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3", len(members))
	}

	// Members come back in submission order, with the details of their job.
	if members[0].Position != 0 || members[0].JobID != "job-0" || members[0].Status != SUCCESSFUL {
		t.Errorf("member 0 = %+v, want job-0 successful", members[0])
	}
	if members[0].ProcessID != "pyecho" || len(members[0].Tags) != 1 {
		t.Errorf("member 0 = %+v, want the job's process and tags", members[0])
	}
	if members[0].LastUpdate == nil {
		t.Error("member 0 has no update time, want the job's")
	}

	// The member whose job could not be created keeps its position and carries
	// the reason instead of a job.
	failed := members[2]
	if failed.Position != 2 || failed.JobID != "" || failed.Status != "" {
		t.Errorf("member 2 = %+v, want position 2 with no job", failed)
	}
	if failed.Error != "aws batch refused the submission" {
		t.Errorf("member 2 error = %q, want the create failure", failed.Error)
	}
	if failed.LastUpdate != nil {
		t.Errorf("member 2 update = %v, want none", failed.LastUpdate)
	}
	if failed.Tags == nil {
		t.Error("member 2 tags are nil, want an empty list so the response is not null")
	}
}

func TestJobGroupMemberFilters(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	cases := []struct {
		name     string
		statuses []string
		want     []int // member positions
	}{
		{name: "by job status", statuses: []string{RUNNING}, want: []int{1}},
		{name: "several job statuses", statuses: []string{RUNNING, SUCCESSFUL}, want: []int{0, 1}},
		{name: "members with no job", statuses: []string{StatusNotCreated}, want: []int{2}},
		{name: "job status and no job together", statuses: []string{RUNNING, StatusNotCreated}, want: []int{1, 2}},
		{name: "a status no member has", statuses: []string{LOST}, want: []int{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			members, err := db.GetJobGroupMembers(rec.GroupID, 10, 0, tc.statuses)
			if err != nil {
				t.Fatalf("GetJobGroupMembers() error: %v", err)
			}
			if len(members) != len(tc.want) {
				t.Fatalf("got %d members, want %d", len(members), len(tc.want))
			}
			for i, position := range tc.want {
				if members[i].Position != position {
					t.Errorf("member %d is position %d, want %d", i, members[i].Position, position)
				}
			}
		})
	}
}

func TestJobGroupMemberPaging(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	first, err := db.GetJobGroupMembers(rec.GroupID, 2, 0, nil)
	if err != nil {
		t.Fatalf("GetJobGroupMembers() error: %v", err)
	}
	second, err := db.GetJobGroupMembers(rec.GroupID, 2, 2, nil)
	if err != nil {
		t.Fatalf("GetJobGroupMembers() error: %v", err)
	}

	if len(first) != 2 || first[0].Position != 0 || first[1].Position != 1 {
		t.Errorf("first page = %v, want positions 0 and 1", positions(first))
	}
	if len(second) != 1 || second[0].Position != 2 {
		t.Errorf("second page = %v, want position 2", positions(second))
	}
}

func TestJobGroupMemberJobIDs(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	ids, err := db.GetJobGroupMemberJobIDs(rec.GroupID)
	if err != nil {
		t.Fatalf("GetJobGroupMemberJobIDs() error: %v", err)
	}

	// Only members that have a job, in submission order, so that dismissing
	// backwards through this list reaches the queued members first.
	if len(ids) != 2 || ids[0] != "job-0" || ids[1] != "job-1" {
		t.Errorf("ids = %v, want [job-0 job-1]", ids)
	}
}

func TestJobGroupSubmissionLifecycle(t *testing.T) {
	db := newTestDB(t)
	rec := newTestGroup(t, db)

	submitting, err := db.GetSubmittingJobGroups()
	if err != nil {
		t.Fatalf("GetSubmittingJobGroups() error: %v", err)
	}
	if len(submitting) != 1 || submitting[0].GroupID != rec.GroupID {
		t.Fatalf("submitting groups = %d, want only %s", len(submitting), rec.GroupID)
	}

	message := "2 of 4 members could not be created"
	if err := db.UpdateJobGroupSubmitted(rec.GroupID, time.Now(), message); err != nil {
		t.Fatalf("UpdateJobGroupSubmitted() error: %v", err)
	}

	// A group that has finished submitting is no longer waiting for members,
	// so a restart must not try to close it out again.
	submitting, err = db.GetSubmittingJobGroups()
	if err != nil {
		t.Fatalf("GetSubmittingJobGroups() error: %v", err)
	}
	if len(submitting) != 0 {
		t.Errorf("submitting groups = %d, want none", len(submitting))
	}

	got, _, err := db.GetJobGroup(rec.GroupID)
	if err != nil {
		t.Fatalf("GetJobGroup() error: %v", err)
	}
	if got.Submitted == nil || got.Message != message {
		t.Errorf("got submitted = %v, message = %q, want both set", got.Submitted, got.Message)
	}

	if err := db.UpdateJobGroupDismissed(rec.GroupID, time.Now()); err != nil {
		t.Fatalf("UpdateJobGroupDismissed() error: %v", err)
	}
	got, _, err = db.GetJobGroup(rec.GroupID)
	if err != nil {
		t.Fatalf("GetJobGroup() error: %v", err)
	}
	if got.Dismissed == nil {
		t.Error("dismissed is unset after dismissing the group")
	}
	// Dismissing must not disturb what submission recorded.
	if got.Submitted == nil || got.Message != message {
		t.Errorf("got submitted = %v, message = %q, want both unchanged", got.Submitted, got.Message)
	}
}

func positions(members []JobGroupMember) []int {
	got := make([]int, len(members))
	for i, m := range members {
		got[i] = m.Position
	}
	return got
}
