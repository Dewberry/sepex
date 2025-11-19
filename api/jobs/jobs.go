package jobs

import (
	"app/utils"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/labstack/gommon/log"
	"github.com/sirupsen/logrus"
)

type Resources struct {
	CPUs   float32
	Memory int
}

// Job refers to any process that has been created through
// the processes/{processID}/execution endpoint
type Job interface {
	CMD() []string
	CurrentStatus() string
	Equals(Job) bool
	IMAGE() string
	JobID() string
	ProcessID() string
	ProcessVersionID() string
	SUBMITTER() string

	// UpdateProcessLogs must provide most upto date process logs
	// for containerized processes, first fetch the current container logs
	// UpdateProcessLogs must update logs stored on the disk
	UpdateProcessLogs() error
	// Kill should successfully send kill signal to the accepted or running container/job
	// Kill should call Close() in new routine. Error in Close() routine does not effect Kill,
	// job is already considered dismissed at this point
	Kill() error
	LastUpdate() time.Time
	LogMessage(string, logrus.Level)

	// NewStatusUpdate must update the status of the job to the provided status string.
	// If a zero-value time is provided as updateTime, the current time (time.Now()) should be set as the UpdateTime.
	// Otherwise, the provided updateTime should be set as the UpdateTime.
	// This function should also update the job record in the database with the new status and UpdateTime.
	// If old status is one of the terminated status, it should not update status.
	NewStatusUpdate(string, time.Time)

	// Create must change job status to accepted.
	// Must create log files.
	// At this point job should be ready to be processed and added to database
	Create() error

	WriteMetaData()
	// WriteResults([]byte) error

	// WaitForRunCompletion must wait until the job is completed.
	WaitForRunCompletion()

	// Decrement Run Waitgroup
	RunFinished()

	// Pefrom any cleanup such as cancelling context etc
	// It is the responsibility of whoever is updating the terminated status to also call Close()
	Close()
}

// JobRecord contains details about a job
type JobRecord struct {
	JobID      string    `json:"jobID"`
	LastUpdate time.Time `json:"updated"`
	Status     string    `json:"status"`
	ProcessID  string    `json:"processID"`
	Type       string    `default:"process" json:"type"`
	Host       string    `json:"host,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Submitter  string    `json:"submitter"`
}

type LogEntry struct {
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Time  time.Time `json:"time"`
}

// Remove empty logs
func DecodeLogStrings(s []string) []LogEntry {
	logs := make([]LogEntry, 0)
	for _, s := range s {
		if s == "" {
			continue
		}
		var log LogEntry
		err := json.Unmarshal([]byte(s), &log)
		if err != nil || (log.Msg == "" && s != "") { // incase log is not valid JSON or log is valid but does not have msg field or have other fields
			log = LogEntry{Msg: s}
		}
		if log.Msg != "" {
			logs = append(logs, log)
		}
	}
	return logs
}

// JobLogs describes logs for the job
type JobLogs struct {
	JobID       string     `json:"jobID"`
	ProcessID   string     `json:"processID"`
	Status      string     `json:"status"`
	ProcessLogs []LogEntry `json:"process_logs"`
	ServerLogs  []LogEntry `json:"server_logs"`
}

// Prettify JobLogs by replacing nil with empty []LogEntry{}
func (jl *JobLogs) Prettify() {
	if jl.ProcessLogs == nil {
		jl.ProcessLogs = []LogEntry{}
	}
	if jl.ServerLogs == nil {
		jl.ServerLogs = []LogEntry{}
	}
}

// OGCStatusCodes
const (
	ACCEPTED   string = "accepted"
	RUNNING    string = "running"
	SUCCESSFUL string = "successful"
	FAILED     string = "failed"
	DISMISSED  string = "dismissed"
)

// FetchResults by parsing logs
// Assumes last log will be results always
func FetchResults(svc *s3.S3, jid string) (interface{}, error) {

	logs, err := FetchLogs(svc, jid, true)
	if err != nil {
		return nil, err
	}

	processLogs := logs.ProcessLogs
	lastLogIdx := len(processLogs) - 1
	if lastLogIdx < 0 {
		return nil, fmt.Errorf("no process logs available")
	}

	lastLog := processLogs[lastLogIdx]
	lastLogMsg := lastLog.Msg
	lastLogMsg = strings.ReplaceAll(lastLogMsg, "'", "\"")

	var data map[string]interface{}
	err = json.Unmarshal([]byte(lastLogMsg), &data)
	if err != nil {
		return nil, fmt.Errorf(`unable to parse results, expected {"plugin_results": {....}}, found : %s. Error: %s`, lastLog, err.Error())
	}

	pluginResults, ok := data["plugin_results"]
	if !ok {
		return nil, fmt.Errorf("'plugin_results' key not found")
	}

	return pluginResults, nil
}

// // If JobID exists but results file doesn't then it raises an error
// // Assumes jobID is valid
// func FetchResults(svc *s3.S3, jid string) (interface{}, error) {
// 	key := fmt.Sprintf("%s/%s.json", os.Getenv("STORAGE_RESULTS_PREFIX"), jid)

// 	exist, err := utils.KeyExists(key, svc)
// 	if err != nil {
// 		return nil, err
// 	}

// 	if !exist {
// 		return nil, fmt.Errorf("not found")
// 	}

// 	data, err := utils.GetS3JsonData(key, svc)
// 	if err != nil {
// 		return nil, err
// 	}

// 	return data, nil
// }

// If JobID exists but metadata file doesn't then it raises an error
// Assumes jobID is valid
func FetchMeta(svc *s3.S3, jid string) (interface{}, error) {
	key := fmt.Sprintf("%s/%s.json", os.Getenv("STORAGE_METADATA_PREFIX"), jid)

	exist, err := utils.KeyExists(key, svc)
	if err != nil {
		return nil, err
	}

	if !exist {
		return nil, fmt.Errorf("not found")
	}

	data, err := utils.GetS3JsonData(key, svc)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Check for logs in local disk and storage svc
// Assumes jobID is valid, if log file doesn't exist then it raises an error
func FetchLogs(svc *s3.S3, jid string, onlyContainer bool) (JobLogs, error) {
	var result JobLogs
	result.JobID = jid
	localDir := os.Getenv("TMP_JOB_LOGS_DIR") // Local directory where logs are stored

	keys := []struct {
		key    string
		target *[]LogEntry
	}{
		{
			"process",
			&result.ProcessLogs,
		},
		{
			"server",
			&result.ServerLogs,
		},
	}

	for _, k := range keys {
		// First, check locally

		if k.key == "server" && onlyContainer {
			continue
		}

		localPath := fmt.Sprintf("%s/%s.%s.jsonl", localDir, jid, k.key)
		if localContent, err := os.ReadFile(localPath); err == nil {
			logStrings := strings.Split(string(localContent), "\n")
			structuredLogs := DecodeLogStrings(logStrings)
			*k.target = structuredLogs
			continue
		}

		// If not found locally, check storage
		storageKey := fmt.Sprintf("%s/%s.%s.jsonl", os.Getenv("STORAGE_LOGS_PREFIX"), jid, k.key)
		exists, err := utils.KeyExists(storageKey, svc)
		if err != nil {
			return JobLogs{}, err
		}
		if !exists {
			return JobLogs{}, fmt.Errorf("%s log file not found on storage", k.key)
		}
		logs, err := utils.GetS3LinesData(storageKey, svc)
		if err != nil {
			return JobLogs{}, fmt.Errorf("failed to read %s logs from storage: %v", k.key, err)
		}
		structuredLogs := DecodeLogStrings(logs)
		*k.target = structuredLogs
	}

	result.Prettify()
	return result, nil
}

// Upload log files from local disk to storage service
func UploadLogsToStorage(svc *s3.S3, jid, pid string) {

	localDir := os.Getenv("TMP_JOB_LOGS_DIR") // Local directory where logs are stored

	keys := []string{
		"process",
		"server",
	}

	for _, k := range keys {
		localPath := fmt.Sprintf("%s/%s.%s.jsonl", localDir, jid, k)
		bytes, err := os.ReadFile(localPath)
		if err != nil {
			log.Error(err.Error())
		}

		storageKey := fmt.Sprintf("%s/%s.%s.jsonl", os.Getenv("STORAGE_LOGS_PREFIX"), jid, k)
		err = utils.WriteToS3(svc, bytes, storageKey, "text/plain", 0)
		if err != nil {
			log.Error(err.Error())
		}
	}
}

func DeleteLocalLogs(svc *s3.S3, jid, pid string) {
	localDir := os.Getenv("TMP_JOB_LOGS_DIR") // Local directory where logs are stored

	// List of log types
	keys := []string{
		"process",
		"server",
	}

	for _, k := range keys {
		localPath := fmt.Sprintf("%s/%s.%s.jsonl", localDir, jid, k)
		err := os.Remove(localPath)
		if err != nil {
			log.Error(fmt.Sprintf("Failed to delete local file %s: %v", localPath, err))
		}
	}
}
