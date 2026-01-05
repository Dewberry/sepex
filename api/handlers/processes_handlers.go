package handlers

import (
	"app/processes"
	"app/utils"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"gopkg.in/yaml.v3"
)

// ProcessListHandler godoc
// @Summary List Available Processes
// @Description [Process List Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_process_list)
// @Tags processes
// @Accept */*
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /processes [get]
func (rh *RESTHandler) ProcessListHandler(c echo.Context) error {
	err := validateFormat(c)
	if err != nil {
		return err
	}

	// to meet ogc api core requirement /req/core/pl-limit-definition
	limitStr := c.QueryParam("limit")
	offsetStr := c.QueryParam("offset")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit > 100 || limit < 1 {
		limit = 20
	}

	offset, err := strconv.Atoi(offsetStr)
	if err != nil || offset < 0 {
		offset = 0
	}

	// instantiate result variable without importing processes pkg
	result := rh.ProcessList.InfoList[0:0]

	if offset < len(rh.ProcessList.InfoList) {
		upperBound := offset + limit
		if upperBound > len(rh.ProcessList.InfoList) {
			upperBound = len(rh.ProcessList.InfoList)
		}
		result = rh.ProcessList.InfoList[offset:upperBound]
	}

	// required by /req/core/process-list-success
	links := make([]link, 0)

	// if offset is not 0
	if offset != 0 {
		lnk := link{
			Href:  fmt.Sprintf("/processes?offset=%v&limit=%v", offset-limit, limit),
			Title: "prev",
		}
		links = append(links, lnk)
	}

	// if limit is not exhausted
	if limit == len(result) {
		lnk := link{
			Href:  fmt.Sprintf("/processes?offset=%v&limit=%v", offset+limit, limit),
			Title: "next",
		}
		links = append(links, lnk)
	}

	output := make(map[string]interface{}, 0)
	output["processes"] = result
	output["links"] = links

	return prepareResponse(c, http.StatusOK, "processes", output)
}

// ProcessDescribeHandler godoc
// @Summary Describe Process Information
// @Description [Process Description Specification](https://docs.ogc.org/is/18-062r2/18-062r2.html#sc_process_description)
// @Tags processes
// @Param processID path string true "example: pyecho"
// @Accept */*
// @Produce json
// @Success 200 {object} processes.processDescription
// @Router /processes/{processID} [get]
func (rh *RESTHandler) ProcessDescribeHandler(c echo.Context) error {
	processID := c.Param("processID")

	err := validateFormat(c)
	if err != nil {
		return err
	}

	p, _, err := rh.ProcessList.Get(processID)
	if err != nil {
		return prepareResponse(c, http.StatusBadRequest, "error", errResponse{Message: err.Error(), HTTPStatus: http.StatusBadRequest})
	}

	description, err := p.Describe()
	if err != nil {
		return prepareResponse(c, http.StatusInternalServerError, "error", errResponse{Message: err.Error(), HTTPStatus: http.StatusInternalServerError})
	}
	return prepareResponse(c, http.StatusOK, "process", description)
}

// AddProcessHandler adds a new process configuration
func (rh *RESTHandler) AddProcessHandler(c echo.Context) error {

	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")

		// non-admins are not allowed
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	processID := c.Param("processID")
	_, _, err := rh.ProcessList.Get(processID)
	if err == nil {
		return prepareResponse(c, http.StatusBadRequest, "error", errResponse{Message: "Process already exist. Use PUT method to update", HTTPStatus: http.StatusBadRequest})
	}

	var newProcess processes.Process

	if err := c.Bind(&newProcess); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "Invalid process data"})
	}

	bodyProcessID := newProcess.Info.ID
	if bodyProcessID != processID {
		return prepareResponse(c, http.StatusBadRequest, "error", errResponse{Message: "Process ID mismatch", HTTPStatus: http.StatusBadRequest})
	}

	err = newProcess.Validate(rh.Config.ResourceLimits.MaxCPUs, rh.Config.ResourceLimits.MaxMemory)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	filename := fmt.Sprintf("%s/%s/%s.yml", pluginsDir, processID, processID)

	data, err := yaml.Marshal(newProcess)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to marshal process data"})
	}

	// Destination directory
	destDir := fmt.Sprintf("%s/%s", pluginsDir, processID)

	// Create the destination directory including all intermediate directories
	err = os.MkdirAll(destDir, 0755)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to deprecate old process"})
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to write process file"})
	}

	rh.ProcessList.List = append(rh.ProcessList.List, newProcess)
	rh.ProcessList.InfoList = append(rh.ProcessList.InfoList, newProcess.Info)

	return c.JSON(http.StatusOK, map[string]string{"message": "Process added successfully"})
}

// UpdateProcessHandler updates an existing process configuration
// Partial Updates are not allowed
func (rh *RESTHandler) UpdateProcessHandler(c echo.Context) error {

	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")

		// non-admins are not allowed
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	processID := c.Param("processID")

	oldProcess, i, err := rh.ProcessList.Get(processID)
	if err != nil {
		return prepareResponse(c, http.StatusBadRequest, "error", errResponse{Message: "Process does not exist", HTTPStatus: http.StatusBadRequest})
	}

	var updatedProcess processes.Process

	if err := c.Bind(&updatedProcess); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "Invalid process data, partial updates are not allowed"})
	}

	if processID != updatedProcess.Info.ID {
		return c.JSON(http.StatusBadRequest, errResponse{Message: "Process ID mismatch"})
	}

	err = updatedProcess.Validate(rh.Config.ResourceLimits.MaxCPUs, rh.Config.ResourceLimits.MaxMemory)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse{Message: err.Error()})
	}

	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	filename := fmt.Sprintf("%s/%s/%s.yml", pluginsDir, processID, processID)

	oldV := oldProcess.Info.Version

	// to do: this should be atomic

	// Destination directory
	destDir := fmt.Sprintf("%s/deprecated/%s", pluginsDir, processID)

	// Create the destination directory including all intermediate directories
	err = os.MkdirAll(destDir, 0755)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to deprecate old process"})
	}

	// Move the file
	err = os.Rename(filename, fmt.Sprintf("%s/%s_%s.yml", destDir, processID, oldV))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to deprecate old process"})
	}

	data, err := yaml.Marshal(updatedProcess)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to marshal process data"})
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to write process file"})
	}

	rh.ProcessList.List[i] = updatedProcess
	rh.ProcessList.InfoList[i] = updatedProcess.Info

	return c.JSON(http.StatusOK, map[string]string{"message": "Process updated successfully"})
}

// DeleteProcessHandler deletes a process configuration
func (rh *RESTHandler) DeleteProcessHandler(c echo.Context) error {

	if rh.Config.AuthLevel > 0 {
		roles := strings.Split(c.Request().Header.Get("X-SEPEX-User-Roles"), ",")

		// non-admins are not allowed
		if !utils.StringInSlice(rh.Config.AdminRoleName, roles) {
			return c.JSON(http.StatusForbidden, errResponse{Message: "Forbidden"})
		}
	}

	processID := c.Param("processID")

	oldProcess, i, err := rh.ProcessList.Get(processID)
	if err != nil {
		return prepareResponse(c, http.StatusBadRequest, "error", errResponse{Message: "Process does not exist", HTTPStatus: http.StatusBadRequest})
	}

	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	filename := fmt.Sprintf("%s/%s/%s.yml", pluginsDir, processID, processID)

	oldV := oldProcess.Info.Version

	// to do: this should be atomic

	// Create the destination directory including all intermediate directories
	destDir := fmt.Sprintf("%s/deprecated/%s", pluginsDir, processID)

	err = os.MkdirAll(destDir, 0755)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to deprecate old process"})
	}

	// Move the file
	err = os.Rename(filename, fmt.Sprintf("%s/%s_%s.yml", destDir, processID, oldV))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse{Message: "Failed to deprecate old process"})
	}

	rh.ProcessList.List = append(rh.ProcessList.List[:i], rh.ProcessList.List[i+1:]...)
	rh.ProcessList.InfoList = append(rh.ProcessList.InfoList[:i], rh.ProcessList.InfoList[i+1:]...)

	return c.JSON(http.StatusOK, map[string]string{"message": "Process deleted successfully"})
}
