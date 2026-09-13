package handlers

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
	"time"

	"app/jobs"
)

// views parses every html view the same way the server does.
func views(t *testing.T) *template.Template {
	t.Helper()

	tmpl, err := template.New("").Funcs(viewFuncMap()).ParseGlob("../views/*.html")
	if err != nil {
		t.Fatalf("views do not parse: %v", err)
	}
	return tmpl
}

// The server parses the views with template.Must at startup, so a view that
// does not parse is a panic on boot rather than a bad page. This catches it
// here instead.
func TestViewsParse(t *testing.T) {
	tmpl := views(t)

	for _, name := range []string{"jobs", "jobStatus", "jobGroup", "resourceStatus", "processes", "landing"} {
		if tmpl.Lookup(name) == nil {
			t.Errorf("view %q was not defined by any file in views/", name)
		}
	}
}

// A view can parse and still fail when it is rendered, because a field it names
// only has to exist at that point. This renders the group view over the
// response the handler actually passes it.
func TestJobGroupViewRenders(t *testing.T) {
	updated := time.Date(2026, 9, 11, 18, 9, 40, 0, time.UTC)

	resp := jobGroupResponse{
		GroupID:   "0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10",
		Status:    jobs.RUNNING,
		Message:   "1 of 3 members could not be created",
		Created:   time.Date(2026, 9, 11, 18, 2, 11, 0, time.UTC),
		Updated:   &updated,
		Submitter: "someone@example.com",
		Tags:      []string{"reach:123"},
		Requested: 3,
		Summary:   jobs.JobGroupSummary{Running: 1, Successful: 1, NotCreated: 1},
		Jobs: []jobs.JobGroupMember{
			{
				Position:   0,
				JobID:      "5a0e2f7c",
				ProcessID:  "pyecho",
				Status:     jobs.SUCCESSFUL,
				LastUpdate: &updated,
				Tags:       []string{"group:0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10"},
			},
			{
				Position: 1,
				JobID:    "9c41b8d3",
				// A member still running has no update time of its own yet.
				ProcessID: "pyecho",
				Status:    jobs.RUNNING,
				Tags:      []string{},
			},
			{
				// A member whose job could not be created has no job at all,
				// which the view has to render without dereferencing anything.
				Position: 2,
				Error:    "aws batch refused the submission",
				Tags:     []string{},
			},
		},
		Links: []link{
			{Href: "/job-groups/0d8f6c2e", Rel: "self"},
			{Href: "/job-groups/0d8f6c2e?offset=20&limit=20", Title: "next"},
		},
	}

	var out bytes.Buffer
	if err := views(t).ExecuteTemplate(&out, "jobGroup", resp); err != nil {
		t.Fatalf("rendering the job group view failed: %v", err)
	}

	body := out.String()
	for _, want := range []string{
		resp.GroupID,
		"running",
		"1 of 3 members could not be created",
		"not created",
		"aws batch refused the submission",
		"group:0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10",
		"Next",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered view does not mention %q", want)
		}
	}
}

// A group with no members yet is the state every group passes through, so the
// view must render it rather than assuming there is something to show.
func TestJobGroupViewRendersEmptyGroup(t *testing.T) {
	resp := jobGroupResponse{
		GroupID:   "0d8f6c2e",
		Status:    jobs.ACCEPTED,
		Created:   time.Now(),
		Submitter: "someone@example.com",
		Tags:      []string{},
		Requested: 500,
		Jobs:      []jobs.JobGroupMember{},
		Links:     []link{{Href: "/job-groups/0d8f6c2e", Rel: "self"}},
	}

	var out bytes.Buffer
	if err := views(t).ExecuteTemplate(&out, "jobGroup", resp); err != nil {
		t.Fatalf("rendering an empty job group failed: %v", err)
	}
	if !strings.Contains(out.String(), "accepted") {
		t.Error("rendered view does not show the group's status")
	}
}
