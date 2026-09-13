package handlers

import (
	"app/jobs"
	"app/utils"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

const (
	// defaultGroupMemberLimit is how many members one page of a group lists.
	defaultGroupMemberLimit = 20
	// maxGroupMemberLimit caps that page. A limit above it is clamped rather
	// than replaced by the default, so that asking for too much returns as much
	// as is allowed instead of silently returning far less than was asked for.
	maxGroupMemberLimit = 100
)

// groupExecutionRequestBody is the payload of a group submission. Tags given
// here belong to the group and are written onto every member.
type groupExecutionRequestBody struct {
	Tags []string          `json:"tags"`
	Jobs []groupJobRequest `json:"jobs"`
}

// groupJobRequest is one member's share of the payload: the inputs it runs
// with, and any tags of its own.
type groupJobRequest struct {
	Inputs map[string]interface{} `json:"inputs"`
	Tags   []string               `json:"tags"`
}

// jobGroupCreatedResponse is what a submission returns. It carries no members:
// they are created in the background, so at this point none exists yet.
type jobGroupCreatedResponse struct {
	GroupID   string    `json:"groupID"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Submitter string    `json:"submitter"`
	Tags      []string  `json:"tags"`
	Requested int       `json:"requested"`
	Links     []link    `json:"links"`
}

// jobGroupResponse reports a group: one combined status, a summary over every
// member, and one page of the members themselves.
type jobGroupResponse struct {
	GroupID   string                `json:"groupID"`
	Status    string                `json:"status"`
	Message   string                `json:"message,omitempty"`
	Created   time.Time             `json:"created"`
	Updated   *time.Time            `json:"updated,omitempty"`
	Submitter string                `json:"submitter"`
	Tags      []string              `json:"tags"`
	Requested int                   `json:"requested"`
	Summary   jobs.JobGroupSummary  `json:"summary"`
	Jobs      []jobs.JobGroupMember `json:"jobs"`
	Links     []link                `json:"links"`
}

func groupHref(groupID string) string {
	return "/job-groups/" + groupID
}

// @Summary Execute Process as a Job Group
// @Description Submits many jobs for one process in a single request and tracks them as one unit. Members are ordinary jobs, created in the background.
// @Tags job-groups
// @Accept json
// @Produce json
// @Param processID path string true "pyecho"
// @Param jobs body string true "example: {tags: [reach:123], jobs: [{inputs: {text: Hello World!}}]}"
// @Success 201 {object} jobGroupCreatedResponse
// @Router /processes/{processID}/group-execution [post]
// Does not produce HTML
func (rh *RESTHandler) GroupExecutionHandler(c echo.Context) error {
	processID := c.Param("processID")
	if processID == "" {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'processID' parameter is required"})
	}

	p, _, err := rh.ProcessList.Get(processID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'processID' incorrect"})
	}

	// The same rule as a single execution: a group is many executions of one
	// process, so it needs the permission that one execution needs.
	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) && !utils.StringInSlice(processID, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	// A group is always asynchronous. Its members are created after the
	// response, so there is no request left to hold open while one runs, and a
	// Prefer header has nothing to choose between.
	if !utils.StringInSlice("async-execute", p.Info.JobControlOptions) {
		return c.JSON(http.StatusUnprocessableEntity, errResponse{
			Message: fmt.Sprintf("process %s does not support async-execute, and a job group is always asynchronous", processID),
		})
	}

	if err := rh.rejectUnschedulableGPUs(p); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errResponse{Message: err.Error()})
	}

	var params groupExecutionRequestBody
	if err := c.Bind(&params); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	// Everything is validated before anything is created, so that a request
	// that cannot be served in full creates nothing at all and can be fixed and
	// sent again.
	if len(params.Jobs) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'jobs' is required in the body of the request and must contain at least one job"})
	}
	if len(params.Jobs) > rh.Config.MaxGroupSize {
		return c.JSON(http.StatusBadRequest, errResponse{
			Message: fmt.Sprintf("a job group may contain at most %d jobs, this request has %d; split it or raise MAX_GROUP_SIZE", rh.Config.MaxGroupSize, len(params.Jobs)),
		})
	}

	if params.Tags == nil {
		params.Tags = []string{}
	}
	if err := utils.SanitizeTags(params.Tags); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}
	if err := rejectReservedTags(params.Tags); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	for i := range params.Jobs {
		entry := &params.Jobs[i]

		if entry.Inputs == nil {
			return c.JSON(http.StatusBadRequest, errResponse{Message: fmt.Sprintf("'inputs' is required for every job in the group, job %d has none", i)})
		}
		if entry.Tags == nil {
			entry.Tags = []string{}
		}
		if err := utils.SanitizeTags(entry.Tags); err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{Message: fmt.Sprintf("job %d: %s", i, err.Error())})
		}
		if err := rejectReservedTags(entry.Tags); err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{Message: fmt.Sprintf("job %d: %s", i, err.Error())})
		}
		if err := p.VerifyInputs(entry.Inputs); err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{Message: fmt.Sprintf("job %d: %s", i, err.Error())})
		}
	}

	rec := jobs.JobGroupRecord{
		GroupID:   uuid.New().String(),
		Submitter: c.Request().Header.Get("X-SEPEX-User-Email"),
		Tags:      params.Tags,
		Requested: len(params.Jobs),
		Created:   time.Now(),
	}

	// Stored before submission starts, so that the group exists for anyone who
	// polls or dismisses it while its members are still being created.
	if err := rh.DB.AddJobGroup(rec); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: fmt.Sprintf("could not create job group: %s", err.Error())})
	}

	rh.GroupSubmitter.Submit(rec, p, params.Jobs)

	c.Response().Header().Set("Location", groupHref(rec.GroupID))
	return c.JSON(http.StatusCreated, jobGroupCreatedResponse{
		GroupID:   rec.GroupID,
		Status:    jobs.ACCEPTED,
		Created:   rec.Created,
		Submitter: rec.Submitter,
		Tags:      rec.Tags,
		Requested: rec.Requested,
		Links:     []link{{Href: groupHref(rec.GroupID), Rel: "self", Type: "application/json"}},
	})
}

// @Summary Job Group Status
// @Description Reports a group's combined status, a summary of its members by status, and one page of members.
// @Tags job-groups
// @Accept */*
// @Produce json
// @Param groupID path string true "example: 0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10"
// @Success 200 {object} jobGroupResponse
// @Router /job-groups/{groupID} [get]
func (rh *RESTHandler) JobGroupStatusHandler(c echo.Context) error {
	if err := validateFormat(c); err != nil {
		return err
	}

	groupID := c.Param("groupID")
	rec, ok, err := rh.DB.GetJobGroup(groupID)
	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}
	if !ok {
		output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("%s job group id not found", groupID)}
		return prepareResponse(c, http.StatusNotFound, "error", output)
	}

	// A group is a list of jobs, so it applies the rule of the job list rather
	// than of a single job: at the strictest auth level a non-admin sees only
	// what they submitted.
	if rh.Config.AuthLevel > 1 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) && rec.Submitter != c.Request().Header.Get("X-SEPEX-User-Email") {
			output := errResponse{HTTPStatus: http.StatusForbidden, Message: "Forbidden"}
			return prepareResponse(c, http.StatusForbidden, "error", output)
		}
	}

	limit, offset, statusList, err := groupMemberQuery(c)
	if err != nil {
		output := errResponse{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
		return prepareResponse(c, http.StatusBadRequest, "error", output)
	}

	resp, err := rh.jobGroupView(rec, limit, offset, statusList)
	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}
	resp.Links = groupLinks(rec.GroupID, limit, offset, c.QueryParam("status"), len(resp.Jobs))

	return prepareResponse(c, http.StatusOK, "jobGroup", resp)
}

// @Summary Dismiss Job Group
// @Description Dismisses every member of a group that is still accepted or running, leaving finished members as they are. Safe to repeat.
// @Tags job-groups
// @Accept */*
// @Produce json
// @Param groupID path string true "example: 0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10"
// @Success 200 {object} jobGroupResponse
// @Router /job-groups/{groupID} [delete]
// Does not produce HTML
func (rh *RESTHandler) JobGroupDismissHandler(c echo.Context) error {
	groupID := c.Param("groupID")

	rec, ok, err := rh.DB.GetJobGroup(groupID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: err.Error()})
	}
	if !ok {
		return c.JSON(http.StatusNotFound, errResponse{Message: fmt.Sprintf("%s job group id not found", groupID)})
	}

	// The same rule as dismissing a single job: the submitter or an admin.
	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")
		if rec.Submitter != c.Request().Header.Get("X-SEPEX-User-Email") && !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	// Stop any submission still in flight first, so that it cannot add members
	// behind the dismissal.
	rh.GroupSubmitter.Cancel(groupID, "the group was dismissed")

	if rec.Dismissed == nil {
		now := time.Now()
		if err := rh.DB.UpdateJobGroupDismissed(groupID, now); err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse{Message: err.Error()})
		}
		rec.Dismissed = &now
	}

	jobIDs, err := rh.DB.GetJobGroupMemberJobIDs(groupID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: err.Error()})
	}

	// Dismissed in reverse submission order, because the members that are
	// running hold the lowest positions. Working backwards empties the queue of
	// this group's members before the first running one releases its resources,
	// so the queue worker cannot start a member that is about to be killed.
	dismissed := 0
	failures := make([]string, 0)
	for i := len(jobIDs) - 1; i >= 0; i-- {
		found, err := rh.dismissActiveJob(jobIDs[i])
		if err != nil {
			// Reported rather than fatal: the rest of the group still has to be
			// dismissed, and repeating the call retries whatever was missed.
			failures = append(failures, fmt.Sprintf("%s (%s)", jobIDs[i], err.Error()))
			continue
		}
		if found {
			dismissed++
		}
	}

	resp, err := rh.jobGroupView(rec, defaultGroupMemberLimit, 0, nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: err.Error()})
	}
	resp.Links = groupLinks(groupID, defaultGroupMemberLimit, 0, "", len(resp.Jobs))

	if len(failures) > 0 {
		note := fmt.Sprintf("dismissed %d member(s); %d could not be dismissed and can be retried by repeating this call: %s",
			dismissed, len(failures), strings.Join(failures, ", "))

		// Added to what the group already says rather than replacing it, so a
		// failed submission is not hidden by a failed dismissal.
		if resp.Message == "" {
			resp.Message = note
		} else {
			resp.Message += ". " + note
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// jobGroupView assembles a group's response: its record, a summary over all of
// its members, and one page of them.
func (rh *RESTHandler) jobGroupView(rec jobs.JobGroupRecord, limit, offset int, statuses []string) (jobGroupResponse, error) {
	summary, latest, err := rh.DB.GetJobGroupSummary(rec.GroupID)
	if err != nil {
		return jobGroupResponse{}, err
	}
	// Counted from what was requested, so that members whose creation failed
	// and members never attempted are both accounted for.
	summary.FillNotCreated(rec.Requested)

	members, err := rh.DB.GetJobGroupMembers(rec.GroupID, limit, offset, statuses)
	if err != nil {
		return jobGroupResponse{}, err
	}

	resp := jobGroupResponse{
		GroupID:   rec.GroupID,
		Status:    jobs.GroupStatus(rec, summary),
		Message:   rec.Message,
		Created:   rec.Created,
		Submitter: rec.Submitter,
		Tags:      rec.Tags,
		Requested: rec.Requested,
		Summary:   summary,
		Jobs:      members,
	}

	// The group's updated time is the latest thing that happened to it: the
	// last member update, or the dismissal when that came later.
	updated := latest
	if rec.Dismissed != nil && rec.Dismissed.After(updated) {
		updated = *rec.Dismissed
	}
	if !updated.IsZero() {
		resp.Updated = &updated
	}

	return resp, nil
}

// groupMemberQuery reads the paging and filter parameters for a group's member
// list.
func groupMemberQuery(c echo.Context) (limit, offset int, statuses []string, err error) {
	limit = defaultGroupMemberLimit
	if limitStr := c.QueryParam("limit"); limitStr != "" {
		parsed, convErr := strconv.Atoi(limitStr)
		if convErr != nil || parsed < 1 {
			return 0, 0, nil, fmt.Errorf("'limit' must be a positive integer")
		}
		if parsed > maxGroupMemberLimit {
			parsed = maxGroupMemberLimit
		}
		limit = parsed
	}

	if offsetStr := c.QueryParam("offset"); offsetStr != "" {
		parsed, convErr := strconv.Atoi(offsetStr)
		if convErr != nil || parsed < 0 {
			return 0, 0, nil, fmt.Errorf("'offset' must be zero or a positive integer")
		}
		offset = parsed
	}

	if statusParam := c.QueryParam("status"); statusParam != "" {
		for _, st := range strings.Split(statusParam, ",") {
			st = strings.TrimSpace(st)
			if st == "" {
				continue
			}
			switch st {
			case jobs.ACCEPTED, jobs.RUNNING, jobs.DISMISSED, jobs.FAILED, jobs.SUCCESSFUL, jobs.LOST, jobs.StatusNotCreated:
				statuses = append(statuses, st)
			default:
				return 0, 0, nil, fmt.Errorf("one or more status values not valid; valid options are %s, %s, %s, %s, %s, %s and %s",
					jobs.ACCEPTED, jobs.RUNNING, jobs.SUCCESSFUL, jobs.FAILED, jobs.DISMISSED, jobs.LOST, jobs.StatusNotCreated)
			}
		}
	}

	return limit, offset, statuses, nil
}

// groupLinks builds the self and paging links for a group. A next link is
// offered when the page came back full, the same way the job list does it,
// which costs no count query.
func groupLinks(groupID string, limit, offset int, status string, returned int) []link {
	links := []link{{Href: groupHref(groupID), Rel: "self", Type: "application/json"}}

	page := func(o int) string {
		href := fmt.Sprintf("%s?offset=%v&limit=%v", groupHref(groupID), o, limit)
		if status != "" {
			href += "&status=" + status
		}
		return href
	}

	if offset != 0 {
		previous := offset - limit
		if previous < 0 {
			previous = 0
		}
		links = append(links, link{Href: page(previous), Title: "prev"})
	}
	if returned == limit {
		links = append(links, link{Href: page(offset + limit), Title: "next"})
	}

	return links
}
