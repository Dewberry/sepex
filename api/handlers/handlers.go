// Package handlers implements echo handler functions.
// It communicates with jobs and processes package to get required resources
package handlers

// Some design decisions:
// Errors should use errResponse, no need to give job status and other details in errors
// Results, Logs, Metadata will reply back with data only no status and other details
// These rules are in compliance with Specs

import (
	"app/jobs"
	"app/utils"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/sirupsen/logrus"
)

// base error
type errResponse struct {
	HTTPStatus int    `json:"-"`
	Message    string `json:"message"`
}

// jobResponse store response of different job endpoints
type jobResponse struct {
	Type       string      `default:"process" json:"type,omitempty"`
	JobID      string      `json:"jobID"`
	LastUpdate time.Time   `json:"updated,omitempty"`
	Status     string      `json:"status,omitempty"`
	ProcessID  string      `json:"processID,omitempty"`
	Message    string      `json:"message,omitempty"`
	Outputs    interface{} `json:"outputs,omitempty"`
}

type link struct {
	Href  string `json:"href"`
	Rel   string `json:"rel,omitempty"`
	Type  string `json:"type,omitempty"`
	Title string `json:"title,omitempty"`
}

// StatusText returns a text for the HTTP status code. It returns the empty
// string if the code is unknown.
func (er errResponse) GetHTTPStatusText() string {
	return http.StatusText(er.HTTPStatus)
}

var validFormats = []string{"", "json", "html"}

// Check if format query parameter is allowed.
func validateFormat(c echo.Context) error {
	outputFormat := c.QueryParam("f")
	if !utils.StringInSlice(outputFormat, validFormats) {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "Invalid option for query parameter 'f'. Valid options are 'html' or 'json'. Default (i.e. not specified) is json for non browser requests and html for browser requests."})
	}
	return nil
}

// Prepare and return response based on query parameter.
// Assumes query parameter is valid.
// If query parameter not defined then fall back to Accept header as suggested in OGC Specs
// Both are not defined then return JSON
func prepareResponse(c echo.Context, httpStatus int, renderName string, output interface{}) error {
	// this is to conform to OGC Process API classes: /req/html/definition and /req/json/definition
	outputFormat := c.QueryParam("f")
	switch outputFormat {
	case "html":
		return c.Render(httpStatus, renderName, output)
	case "json":
		return c.JSON(httpStatus, output)
	default:
		accept := c.Request().Header.Get("Accept")
		if strings.Contains(accept, "application/json") {
			// Browsers generally send text/html as an accept header
			// return c.Render(httpStatus, renderName, output)
			return c.JSON(httpStatus, output)
		} else if strings.Contains(accept, "text/html") {
			// Browsers generally send text/html as an accept header
			return c.Render(httpStatus, renderName, output)
		} else {
			// Default to JSON for any other cases, including 'Accept: */*'
			return c.JSON(httpStatus, output)
		}
	}
}

// runRequestBody provides the required inputs for containerized processes
// specs: https://developer.ogc.org/api/processes/index.html#tag/Execute
type runRequestBody struct {
	Inputs map[string]interface{} `json:"inputs"`
}

// LandingPage godoc
// @Summary Landing Page
// @Description [LandingPage Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_landing_page)
// @Tags info
// @Accept */*
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router / [get]
func (rh *RESTHandler) LandingPage(c echo.Context) error {
	err := validateFormat(c)
	if err != nil {
		return err
	}

	output := map[string]string{
		"title":       rh.Title,
		"description": rh.Description,
	}
	return prepareResponse(c, http.StatusOK, "landing", output)
}

// Conformance godoc
// @Summary API Conformance List
// @Description [Conformance Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_conformance_classes)
// @Tags info
// @Accept */*
// @Produce json
// @Success 200 {object} map[string]interface{} "conformsTo:["http://schemas.opengis.net/ogcapi/processes/part1/1.0/openapi/...."]"
// @Router /conformance [get]
func (rh *RESTHandler) Conformance(c echo.Context) error {
	err := validateFormat(c)
	if err != nil {
		return err
	}

	output := map[string][]string{
		"conformsTo": rh.ConformsTo,
	}

	return prepareResponse(c, http.StatusOK, "conformance", output)
}

// @Summary Execute Process
// @Description [Execute Process Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_create_job)
// @Tags processes
// @Accept json
// @Produce json
// @Param processID path string true "pyecho"
// @Param inputs body string true "example: {inputs: {text:Hello World!}} (add double quotes for all strings in the payload)"
// @Success 200 {object} jobResponse
// @Router /processes/{processID}/execution [post]
// Does not produce HTML
func (rh *RESTHandler) Execution(c echo.Context) error {
	processID := c.Param("processID")

	if processID == "" {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'processID' parameter is required"})
	}

	p, _, err := rh.ProcessList.Get(processID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'processID' incorrect"})
	}

	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-ProcessAPI-User-Roles"), ",")

		// admins are allowed to execute all processes, else you need to have a role with same name as processId
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) && !utils.StringInSlice(processID, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	var params runRequestBody
	err = c.Bind(&params)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	if params.Inputs == nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "'inputs' is required in the body of the request"})
	}

	err = p.VerifyInputs(params.Inputs)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	jsonParams, err := json.Marshal(params.Inputs)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: err.Error()})
	}

	// If `"Inputs": {}` in `/execution` payload. Nothing will be appended to process commands.
	// This allow running processes that do not have any inputs.
	var cmd = []string{}
	if p.Command != nil {
		cmd = append(cmd, p.Command...)
	}
	if string(jsonParams) != "{}" {
		cmd = append(cmd, string(jsonParams))
	}

	mode := p.Info.JobControlOptions[0]
	host := p.Host.Type

	// ----------- Process related setup is complete at this point ---------

	jobID := uuid.New().String()

	// switch host {
	// case "docker":
	// 	params.Inputs["resultsCallbackUri"] = fmt.Sprintf("%s/jobs/%s/results_update", os.Getenv("API_URL_LOCAL"), jobID)
	// default:
	// 	params.Inputs["resultsCallbackUri"] = fmt.Sprintf("%s/jobs/%s/results_update", os.Getenv("API_URL_PUBLIC"), jobID)
	// }

	submitter := c.Request().Header.Get("X-ProcessAPI-User-Email")
	var j jobs.Job
	switch host {
	case "docker":
		j = &jobs.DockerJob{
			UUID:           jobID,
			ProcessName:    processID,
			ProcessVersion: p.Info.Version,
			Image:          p.Host.Image,
			Submitter:      submitter,
			EnvVars:        p.Config.EnvVars,
			Volumes:        p.Config.Volumes,
			Resources:      jobs.Resources(p.Config.Resources),
			Cmd:            cmd,
			StorageSvc:     rh.StorageSvc,
			DB:             rh.DB,
			DoneChan:       rh.MessageQueue.JobDone,
		}

	case "aws-batch":
		j = &jobs.AWSBatchJob{
			UUID:           jobID,
			ProcessName:    processID,
			Image:          p.Host.Image,
			Submitter:      submitter,
			EnvVars:        p.Config.EnvVars,
			Cmd:            cmd,
			JobDef:         p.Host.JobDefinition,
			JobQueue:       p.Host.JobQueue,
			JobName:        fmt.Sprintf("%s_%s", rh.Name, jobID),
			ProcessVersion: p.Info.Version,
			StorageSvc:     rh.StorageSvc,
			DB:             rh.DB,
			DoneChan:       rh.MessageQueue.JobDone,
		}

	case "subprocess":
		j = &jobs.SubprocessJob{
			UUID:           jobID,
			ProcessName:    processID,
			Submitter:      submitter,
			EnvVars:        p.Config.EnvVars,
			Cmd:            cmd,
			ProcessVersion: p.Info.Version,
			StorageSvc:     rh.StorageSvc,
			DB:             rh.DB,
			DoneChan:       rh.MessageQueue.JobDone,
		}
	}

	// Create job
	err = j.Create()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: fmt.Sprintf("submission error %s", err.Error())})
	}

	// Add to active jobs
	rh.ActiveJobs.Add(&j)

	resp := jobResponse{ProcessID: j.ProcessID(), Type: "process", JobID: jobID, Status: j.CurrentStatus()}
	switch mode {
	case "sync-execute":
		j.WaitForRunCompletion()
		resp.Status = j.CurrentStatus()

		if resp.Status == "successful" {
			var outputs interface{}

			if p.Outputs != nil {
				outputs, err = jobs.FetchResults(rh.StorageSvc, j.JobID())
				if err != nil {
					resp.Message = "error fetching results. Error: " + err.Error()
					return c.JSON(http.StatusInternalServerError, resp)
				}
			}
			resp.Outputs = outputs
			return c.JSON(http.StatusOK, resp)
		} else {
			resp.Message = "job unsuccessful. Call logs route for details"
			return c.JSON(http.StatusInternalServerError, resp)
		}
	case "async-execute":
		resp.Status = j.CurrentStatus()
		return c.JSON(http.StatusCreated, resp)
	default:
		resp := jobResponse{ProcessID: j.ProcessID(), Type: "process", JobID: jobID, Status: "0", Message: "incorrect controller option defined in process configuration"}
		return c.JSON(http.StatusInternalServerError, resp)
	}
}

// @Summary Dismiss Job
// @Description [Dismss Job Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#ats_dismiss)
// @Tags jobs
// @Accept */*
// @Produce json
// @Success 200 {object} jobResponse
// @Router /jobs/{jobID} [delete]
// Does not produce HTML
func (rh *RESTHandler) JobDismissHandler(c echo.Context) error {

	jobID := c.Param("jobID")
	if j, ok := rh.ActiveJobs.Jobs[jobID]; ok {

		if rh.Config.AuthLevel > 0 {
			roles := strings.Split(c.Request().Header.Get("X-ProcessAPI-User-Roles"), ",")

			if (*j).SUBMITTER() != c.Request().Header.Get("X-ProcessAPI-User-Email") && !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
				return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
			}
		}

		err := (*j).Kill()
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
		}
		return c.JSON(http.StatusOK, jobResponse{ProcessID: (*j).ProcessID(), Type: "process", JobID: jobID, Status: (*j).CurrentStatus(), Message: fmt.Sprintf("job %s dismissed", jobID)})
	}
	return c.JSON(http.StatusNotFound, errResponse{Message: fmt.Sprintf("job %s not in the active jobs list", jobID)})
}

// @Summary Job Status
// @Description [Job Status Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_retrieve_status_info)
// @Tags jobs
// @Info [Format YAML](http://schemas.opengis.net/ogcapi/processes/part1/1.0/openapi/schemas/statusInfo.yaml)
// @Accept */*
// @Param jobID path string true "example: 44d9ca0e-2ca7-4013-907f-a8ccc60da3b4"
// @Produce json
// @Success 200 {object} jobResponse
// @Router /jobs/{jobID} [get]
func (rh *RESTHandler) JobStatusHandler(c echo.Context) (err error) {
	err = validateFormat(c)
	if err != nil {
		return err
	}

	var jRcrd jobs.JobRecord
	jobID := c.Param("jobID")
	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok {
		resp := jobResponse{
			ProcessID:  (*job).ProcessID(),
			JobID:      (*job).JobID(),
			LastUpdate: (*job).LastUpdate(),
			Status:     (*job).CurrentStatus(),
		}
		return prepareResponse(c, http.StatusOK, "jobStatus", resp)
	} else if jRcrd, ok, err = rh.DB.GetJob(jobID); ok {
		resp := jobResponse{
			ProcessID:  jRcrd.ProcessID,
			JobID:      jRcrd.JobID,
			LastUpdate: jRcrd.LastUpdate,
			Status:     jRcrd.Status,
		}
		return prepareResponse(c, http.StatusOK, "jobStatus", resp)
	}

	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}
	output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("%s job id not found", jobID)}
	return prepareResponse(c, http.StatusNotFound, "error", output)
}

// @Summary Job Results
// @Description [Job Results Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_retrieve_job_results)
// @Tags jobs
// @Accept */*
// @Produce json
// @Param jobID path string true "ex: 44d9ca0e-2ca7-4013-907f-a8ccc60da3b4"
// @Success 200 {object} map[string]interface{}
// @Router /jobs/{jobID}/results [get]
// Does not produce HTML
func (rh *RESTHandler) JobResultsHandler(c echo.Context) (err error) {
	err = validateFormat(c)
	if err != nil {
		return err
	}

	var jRcrd jobs.JobRecord
	jobID := c.Param("jobID")
	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok { // ActiveJobs hit
		output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("results not ready, job %s", (*job).CurrentStatus())}
		return prepareResponse(c, http.StatusNotFound, "error", output)

	} else if jRcrd, ok, err = rh.DB.GetJob(jobID); ok { // db hit

		switch jRcrd.Status {
		case jobs.SUCCESSFUL:
			outputs, err := jobs.FetchResults(rh.StorageSvc, jRcrd.JobID)
			if err != nil {
				if err.Error() == "not found" {
					output := errResponse{HTTPStatus: http.StatusNotFound, Message: "results not available"}
					return prepareResponse(c, http.StatusNotFound, "error", output)
				}
				output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
				return prepareResponse(c, http.StatusInternalServerError, "error", output)
			}
			output := jobResponse{JobID: jobID, Outputs: outputs}
			return prepareResponse(c, http.StatusOK, "jobResults", output)

		case jobs.FAILED, jobs.DISMISSED:
			output := errResponse{HTTPStatus: http.StatusNotFound, Message: "job Failed or Dismissed. Call logs route for details"}
			return prepareResponse(c, http.StatusNotFound, "error", output)

		default:
			output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: "job status out of sync in database"}
			return prepareResponse(c, http.StatusInternalServerError, "error", output)
		}

	}

	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}

	// miss
	output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("%s job id not found", jobID)}
	return prepareResponse(c, http.StatusNotFound, "error", output)
}

// @Summary Job Metadata
// @Description Provides metadata associated with a job
// @Tags jobs
// @Accept */*
// @Produce json
// @Param jobID path string true "example: 44d9ca0e-2ca7-4013-907f-a8ccc60da3b4"
// @Success 200 {object} map[string]interface{}
// @Router /jobs/{jobID}/results [get]
// Does not produce HTML
func (rh *RESTHandler) JobMetaDataHandler(c echo.Context) (err error) {
	err = validateFormat(c)
	if err != nil {
		return err
	}

	var jRcrd jobs.JobRecord

	jobID := c.Param("jobID")
	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok { // ActiveJobs hit
		output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("metadata not ready, job %s", (*job).CurrentStatus())}
		return prepareResponse(c, http.StatusNotFound, "error", output)

	} else if jRcrd, ok, err = rh.DB.GetJob(jobID); ok { // db hit
		switch jRcrd.Status {
		case jobs.SUCCESSFUL:
			md, err := jobs.FetchMeta(rh.StorageSvc, jobID)
			if err != nil {
				if err.Error() == "not found" {
					output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: "metadata not found"}
					return prepareResponse(c, http.StatusInternalServerError, "error", output)
				}
				output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
				return prepareResponse(c, http.StatusInternalServerError, "error", output)
			}
			return prepareResponse(c, http.StatusOK, "jobMetadata", md)

		case jobs.FAILED, jobs.DISMISSED:
			output := errResponse{HTTPStatus: http.StatusNotFound, Message: "job Failed or Dismissed. Metadata only available for successful jobs"}
			return prepareResponse(c, http.StatusNotFound, "error", output)

		default:
			output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: "job status out of sync in database"}
			return prepareResponse(c, http.StatusInternalServerError, "error", output)
		}

	}

	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}

	// miss
	output := errResponse{HTTPStatus: http.StatusNotFound, Message: fmt.Sprintf("%s job id not found", jobID)}
	return prepareResponse(c, http.StatusNotFound, "error", output)
}

// @Summary Job Logs
// @Description
// @Tags jobs
// @Accept */*
// @Produce json
// @Param jobID path string true "example: 44d9ca0e-2ca7-4013-907f-a8ccc60da3b4"
// @Success 200 {object} jobs.JobLogs
// @Router /jobs/{jobID}/logs [get]
func (rh *RESTHandler) JobLogsHandler(c echo.Context) (err error) {
	jobID := c.Param("jobID")

	err = validateFormat(c)
	if err != nil {
		return err
	}

	var pid, status string
	var jRcrd jobs.JobRecord

	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok { // ActiveJobs hit
		pid = (*job).ProcessID()
		status = (*job).CurrentStatus()
		if status == jobs.ACCEPTED { // this prevents AWS Cloudwatch errors where logs are not available till some time after job is started
			output := errResponse{HTTPStatus: http.StatusBadRequest, Message: "Logs will be available after the job has reached running state."}
			return prepareResponse(c, http.StatusBadRequest, "error", output)
		}

		_ = (*job).UpdateProcessLogs()
	} else if jRcrd, ok, err = rh.DB.GetJob(jobID); ok { // db hit
		pid = jRcrd.ProcessID
		status = jRcrd.Status

		if err != nil {
			output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
			return prepareResponse(c, http.StatusInternalServerError, "error", output)
		}
	} else { // miss
		output := errResponse{HTTPStatus: http.StatusNotFound, Message: "jobID not found"}
		return prepareResponse(c, http.StatusNotFound, "error", output)
	}

	logs, err := jobs.FetchLogs(rh.StorageSvc, jobID, false)
	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: "error while fetching logs: " + err.Error()}
		return prepareResponse(c, http.StatusInternalServerError, "error", output)
	}

	logs.ProcessID = pid
	logs.Status = status
	return prepareResponse(c, http.StatusOK, "jobLogs", logs)

}

// @Summary Summary of all (active) Jobs
// @Description [Job List Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_retrieve_job_results)
// @Tags jobs
// @Accept */*
// @Produce json
// @Success 200 {object} []jobs.JobRecord
// @Router /jobs [get]
func (rh *RESTHandler) ListJobsHandler(c echo.Context) error {
	err := validateFormat(c)
	if err != nil {
		return err
	}

	limitStr := c.QueryParam("limit")
	offsetStr := c.QueryParam("offset")
	processIDs := c.QueryParam("processID") // assuming comma-separated list: "process1,process2"
	statuses := c.QueryParam("status")
	submitters := c.QueryParam("submitter")

	var processIDList []string
	if processIDs != "" {
		processIDList = strings.Split(processIDs, ",")
	}

	var statusList []string
	if statuses != "" {
		statusList = strings.Split(statuses, ",")
	}
	for _, st := range statusList {
		switch st {
		case jobs.ACCEPTED, jobs.RUNNING, jobs.DISMISSED, jobs.FAILED, jobs.SUCCESSFUL:
			// valid status
		default:
			output := errResponse{HTTPStatus: http.StatusBadRequest, Message: "One or more status values not valid"}
			return prepareResponse(c, http.StatusBadRequest, "error", output)
		}
	}

	if rh.Config.AuthLevel > 1 { // changed for hotfix, should be > 0 when clients are updated
		roles := strings.Split(c.Request().Header.Get("X-ProcessAPI-User-Roles"), ",")

		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			submitters = c.Request().Header.Get("X-ProcessAPI-User-Email")
		}
	}

	var submittersList []string
	if submitters != "" {
		submittersList = strings.Split(submitters, ",")
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit > 100 || limit < 1 {
		limit = 20
	}

	offset, err := strconv.Atoi(offsetStr)
	if err != nil || offset < 0 {
		offset = 0
	}

	result, err := rh.DB.GetJobs(limit, offset, processIDList, statusList, submittersList)
	if err != nil {
		output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
		return prepareResponse(c, http.StatusNotFound, "error", output)
	}

	links := make([]link, 0)
	if offset != 0 {
		lnk := link{
			Href:  fmt.Sprintf("/jobs?offset=%v&limit=%v&processID=%v&status=%v&submitter=%v", offset-limit, limit, processIDs, statuses, submitters),
			Title: "prev",
		}
		links = append(links, lnk)
	}
	if limit == len(result) {
		lnk := link{
			Href:  fmt.Sprintf("/jobs?offset=%v&limit=%v&processID=%v&status=%v&submitter=%v", offset+limit, limit, processIDs, statuses, submitters),
			Title: "next",
		}
		links = append(links, lnk)
	}

	output := make(map[string]interface{}, 0)
	output["jobs"] = result
	output["links"] = links
	return prepareResponse(c, http.StatusOK, "jobs", output)
}

// Sample message body:
//
//	{
//		"status": "successful",
//		"updated": "2023-08-28T18:25:44.731Z"
//	}
//
// Time must be in RFC3339(ISO) format
func (rh *RESTHandler) JobStatusUpdateHandler(c echo.Context) error {
	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-ProcessAPI-User-Roles"), ",")

		// only service accounts or admins can post status updates
		if !utils.StringInSlice(rh.Config.ServiceRoleName, roles) && !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	jobID := c.Param("jobID")

	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok { // ActiveJobs hit
		var sm jobs.StatusMessage
		sm.Job = job
		// setup some kind of token/auth to allow only the allowed agents to post to this route
		defer c.Request().Body.Close()
		dataBytes, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{http.StatusBadRequest, "could not read message body"})
		}
		if err = json.Unmarshal(dataBytes, &sm); err != nil {
			return c.JSON(http.StatusBadRequest, errResponse{http.StatusBadRequest, "incorrect message body"})
		}
		// check status valid
		switch sm.Status {
		case jobs.ACCEPTED, jobs.RUNNING, jobs.DISMISSED, jobs.FAILED, jobs.SUCCESSFUL:
			// do nothing
		default:
			return c.JSON(http.StatusBadRequest, fmt.Sprintf("status not valid, valid options are: %s, %s, %s, %s, %s", jobs.ACCEPTED, jobs.RUNNING, jobs.DISMISSED, jobs.FAILED, jobs.SUCCESSFUL))
		}
		(*sm.Job).LogMessage(fmt.Sprintf("Status update received: %s.", sm.Status), logrus.InfoLevel)
		rh.MessageQueue.StatusChan <- sm
		return c.JSON(http.StatusAccepted, "status update received")
	} else if ok, err := rh.DB.CheckJobExist(jobID); ok || err != nil { // db hit or error
		if ok {
			log.Infof("Status update received for inactive job: %s", jobID)
			// returning Accepted here so that callers do not retry
			return c.JSON(http.StatusAccepted, "job not an active job")
		}

		if err != nil {
			output := errResponse{HTTPStatus: http.StatusInternalServerError, Message: err.Error()}
			return prepareResponse(c, http.StatusInternalServerError, "error", output)
		}
	}
	return c.JSON(http.StatusBadRequest, "job id not found")
}

// func (rh *RESTHandler) JobResultsUpdateHandler(c echo.Context) error {

// 	// setup some kind of token/auth to allow only the trusted clients to post to this route

// 	defer c.Request().Body.Close()
// 	dataBytes, err := io.ReadAll(c.Request().Body)
// 	if err != nil {
// 		return c.JSON(http.StatusBadRequest, errResponse{http.StatusBadRequest, "incorrect message body"})
// 	}

// 	jobID := c.Param("jobID")

// 	if job, ok := rh.ActiveJobs.Jobs[jobID]; ok { // ActiveJobs hit
// 		err = (*job).WriteResults(dataBytes)
// 		if err != nil {
// 			return c.JSON(http.StatusInternalServerError, errResponse{http.StatusInternalServerError, "error writing results"})
// 		}
// 		return c.JSON(http.StatusCreated, "results processed")
// 	} else {
// 		return c.JSON(http.StatusBadRequest, "job not an active job")
// 	}

// }
