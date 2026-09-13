package jobs

import (
	"testing"
	"time"
)

// submitted is a group whose members have all been created, meaning nothing
// further is coming.
func submitted() JobGroupRecord {
	t := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	return JobGroupRecord{GroupID: "g", Requested: 10, Submitted: &t}
}

// submitting is a group whose members are still being created.
func submitting() JobGroupRecord {
	return JobGroupRecord{GroupID: "g", Requested: 10}
}

func TestGroupStatus(t *testing.T) {
	cases := []struct {
		name    string
		rec     JobGroupRecord
		summary JobGroupSummary
		want    string
	}{
		// While submission is still running the group is never terminal,
		// however the members created so far have turned out.
		{
			name: "submitting, nothing created yet",
			rec:  submitting(),
			want: ACCEPTED,
		},
		{
			name:    "submitting, every member created so far is queued",
			rec:     submitting(),
			summary: JobGroupSummary{Accepted: 4},
			want:    ACCEPTED,
		},
		{
			name:    "submitting, some members have already finished",
			rec:     submitting(),
			summary: JobGroupSummary{Successful: 4},
			want:    RUNNING,
		},
		{
			name:    "submitting, all created members failed",
			rec:     submitting(),
			summary: JobGroupSummary{Failed: 4},
			want:    RUNNING,
		},

		// Once nothing further is coming, the group is as good as its worst
		// member.
		{
			name:    "queued",
			rec:     submitted(),
			summary: JobGroupSummary{Accepted: 10},
			want:    ACCEPTED,
		},
		{
			name:    "running and queued",
			rec:     submitted(),
			summary: JobGroupSummary{Running: 3, Accepted: 7},
			want:    RUNNING,
		},
		{
			name:    "a failure does not end the group early",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 2, Failed: 1, Running: 4, Accepted: 3},
			want:    RUNNING,
		},
		{
			name:    "all successful",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 10},
			want:    SUCCESSFUL,
		},
		{
			name:    "one member failed",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 9, Failed: 1},
			want:    FAILED,
		},
		{
			name:    "a lost member counts as a failure",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 9, Lost: 1},
			want:    FAILED,
		},
		{
			name:    "dismissed members and nothing failed",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 6, Dismissed: 4},
			want:    DISMISSED,
		},
		{
			name:    "a failure outranks a dismissal",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 7, Dismissed: 2, Failed: 1},
			want:    FAILED,
		},
		{
			name:    "a member that was never created is a failure of the group",
			rec:     submitted(),
			summary: JobGroupSummary{Successful: 9, NotCreated: 1},
			want:    FAILED,
		},
		{
			name:    "no member could be created",
			rec:     submitted(),
			summary: JobGroupSummary{NotCreated: 10},
			want:    FAILED,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupStatus(tc.rec, tc.summary); got != tc.want {
				t.Errorf("GroupStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFillNotCreated(t *testing.T) {
	cases := []struct {
		name      string
		summary   JobGroupSummary
		requested int
		want      int
	}{
		{
			name:      "every member exists",
			summary:   JobGroupSummary{Successful: 7, Running: 3},
			requested: 10,
			want:      0,
		},
		{
			name:      "some members were never created",
			summary:   JobGroupSummary{Successful: 6, Failed: 1},
			requested: 10,
			want:      3,
		},
		{
			name:      "nothing was created",
			requested: 10,
			want:      10,
		},
		{
			// Not reachable through the API, since members are only ever
			// created from the request that fixed the count, but the count
			// reported must never be negative.
			name:      "more members than were requested",
			summary:   JobGroupSummary{Successful: 12},
			requested: 10,
			want:      0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.summary
			s.FillNotCreated(tc.requested)
			if s.NotCreated != tc.want {
				t.Errorf("NotCreated = %d, want %d", s.NotCreated, tc.want)
			}
		})
	}
}

func TestSplitMemberStatusFilter(t *testing.T) {
	statuses, notCreated := splitMemberStatusFilter([]string{RUNNING, StatusNotCreated, FAILED})

	if !notCreated {
		t.Error("expected the not created filter to be recognised")
	}
	if len(statuses) != 2 || statuses[0] != RUNNING || statuses[1] != FAILED {
		t.Errorf("job statuses = %v, want [%s %s]", statuses, RUNNING, FAILED)
	}

	// The job statuses must come back empty rather than carrying a value no
	// job can ever have, which would match nothing and silently return no
	// members.
	statuses, notCreated = splitMemberStatusFilter([]string{StatusNotCreated})
	if !notCreated || len(statuses) != 0 {
		t.Errorf("job statuses = %v, notCreated = %v, want [] true", statuses, notCreated)
	}
}

func TestGroupTag(t *testing.T) {
	groupID := "0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10"

	got := GroupTag(groupID)
	if want := "group:" + groupID; got != want {
		t.Errorf("GroupTag() = %q, want %q", got, want)
	}

	// The tag has to survive the same validation a client's tags go through,
	// or members could not carry it.
	if err := sanitizeCheck(got); err != nil {
		t.Errorf("group tag is not a valid tag: %v", err)
	}
}

// sanitizeCheck mirrors the tag character rule in utils.SanitizeTags. It is
// duplicated rather than imported because utils imports nothing from here and
// this package is not imported by utils either way.
func sanitizeCheck(tag string) error {
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':':
		default:
			return errUnsupportedTagChar
		}
	}
	return nil
}

var errUnsupportedTagChar = &tagError{"tag contains unsupported characters"}

type tagError struct{ msg string }

func (e *tagError) Error() string { return e.msg }
