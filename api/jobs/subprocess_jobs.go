package jobs

import (
	"app/utils"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/sirupsen/logrus"
)

type SubprocessJob struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	// Used for monitoring meta data and other routines
	wg sync.WaitGroup
	// Used for monitoring running complete for sync jobs
	wgRun sync.WaitGroup
	// closeOnce ensures Close() body executes exactly once
	closeOnce sync.Once

	UUID           string `json:"jobID"`
	PID            string
	ProcessName    string `json:"processID"`
	ProcessVersion string `json:"processVersion"`
	Submitter      string
	EnvVars        []string
	Cmd            []string `json:"commandOverride"`
	UpdateTime     time.Time
	Status         string `json:"status"`

	execCmd *exec.Cmd

	logger  *log.Logger
	logFile *os.File

	Resources
	DB           Database
	StorageSvc   *s3.S3
	DoneChan     chan Job
	ResourcePool *ResourcePool
	IsSync       bool
}

func (j *SubprocessJob) WaitForRunCompletion() {
	j.wgRun.Wait()
}

func (j *SubprocessJob) JobID() string {
	return j.UUID
}

func (j *SubprocessJob) ProcessID() string {
	return j.ProcessName
}

func (j *SubprocessJob) ProcessVersionID() string {
	return j.ProcessVersion
}

func (j *SubprocessJob) SUBMITTER() string {
	return j.Submitter
}

func (j *SubprocessJob) CMD() []string {
	return j.Cmd
}

func (j *SubprocessJob) GetResources() Resources {
	return j.Resources
}

func (j *SubprocessJob) LogMessage(m string, level log.Level) {
	switch level {
	case 2:
		j.logger.Error(m)
	case 3:
		j.logger.Warn(m)
	case 4:
		j.logger.Info(m)
	case 5:
		j.logger.Debug(m)
	case 6:
		j.logger.Trace(m)
	default:
		j.logger.Info(m) // default to Info level if level is out of range
	}
}

func (j *SubprocessJob) LastUpdate() time.Time {
	return j.UpdateTime
}

func (j *SubprocessJob) NewStatusUpdate(status string, updateTime time.Time) {

	// If old status is one of the terminated status, it should not update status.
	switch j.Status {
	case SUCCESSFUL, DISMISSED, FAILED:
		return
	}

	j.Status = status
	if updateTime.IsZero() {
		j.UpdateTime = time.Now()
	} else {
		j.UpdateTime = updateTime
	}
	j.DB.updateJobRecord(j.UUID, status, j.UpdateTime)
	j.logger.Infof("Status changed to %s.", status)
}

func (j *SubprocessJob) CurrentStatus() string {
	return j.Status
}

func (j *SubprocessJob) ProviderID() string {
	return j.PID
}

func (j *SubprocessJob) Equals(job Job) bool {
	switch jj := job.(type) {
	case *SubprocessJob:
		return j.ctx == jj.ctx
	default:
		return false
	}
}

func (j *SubprocessJob) initLogger() error {
	// Create a place holder file for subprocess logs
	file, err := os.Create(fmt.Sprintf("%s/%s.process.jsonl", os.Getenv("TMP_JOB_LOGS_DIR"), j.UUID))
	if err != nil {
		return fmt.Errorf("failed to open log file: %s", err.Error())
	}
	file.Close()

	// Create logger for server logs
	j.logger = log.New()

	file, err = os.Create(fmt.Sprintf("%s/%s.server.jsonl", os.Getenv("TMP_JOB_LOGS_DIR"), j.UUID))
	if err != nil {
		return fmt.Errorf("failed to open log file: %s", err.Error())
	}

	j.logger.SetOutput(file)
	j.logger.SetFormatter(&log.JSONFormatter{})

	lvl, err := log.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		j.logger.Warnf("Invalid LOG_LEVEL set, %s; defaulting to INFO", os.Getenv("LOG_LEVEL"))
		lvl = log.InfoLevel
	}
	j.logger.SetLevel(lvl)
	return nil
}

func (j *SubprocessJob) Create() error {
	// Only reserve resources for sync jobs at creation time
	// Async jobs will have resources reserved when QueueWorker starts them
	if j.IsSync {
		if !j.ResourcePool.TryReserve(j.Resources.CPUs, j.Resources.Memory) {
			return fmt.Errorf("resources unavailable")
		}
	}

	// Track if creation succeeded to handle cleanup on error
	success := false
	defer func() {
		if !success && j.IsSync {
			j.ResourcePool.Release(j.Resources.CPUs, j.Resources.Memory)
		}
	}()

	err := j.initLogger()
	if err != nil {
		return err
	}
	j.logger.Info("Subprocess Commands: ", j.CMD())

	ctx, cancelFunc := context.WithCancel(context.TODO())
	j.ctx = ctx
	j.ctxCancel = cancelFunc

	// At this point job is ready to be added to database
	err = j.DB.addJob(j.UUID, "accepted", "", "local", j.ProcessName, j.Submitter, time.Now())
	if err != nil {
		j.ctxCancel()
		return err
	}

	j.NewStatusUpdate(ACCEPTED, time.Time{})

	// Increment wgRun here so WaitForRunCompletion() blocks
	// even if QueueWorker hasn't called StartRun() yet
	j.wgRun.Add(1)

	success = true
	return nil
}

func (j *SubprocessJob) IsSyncJob() bool {
	return j.IsSync
}

func (j *SubprocessJob) Run() {
	// Single consolidated defer for all cleanup operations.
	// Order of operations:
	//   1. Recover from panic (if any) and mark job as FAILED
	//   2. Release resources - free CPU/memory for next job in queue
	//   3. Close() - cleanup process, logs, remove from ActiveJobs
	//      (closeOnce guarantees this only executes once, even if Kill() also called Close())
	//   4. wgRun.Done() - unblock sync job waiters after results are available
	defer func() {
		if r := recover(); r != nil {
			j.logger.Errorf("Run() panicked: %v", r)
			j.NewStatusUpdate(FAILED, time.Time{})
		}
		j.ResourcePool.Release(j.Resources.CPUs, j.Resources.Memory)
		j.Close()
		j.wgRun.Done()
	}()

	// Prepare the command
	j.execCmd = exec.CommandContext(j.ctx, j.Cmd[0], j.Cmd[1:]...)

	envs := make([]string, len(j.EnvVars))
	for i, k := range j.EnvVars {
		name := strings.TrimPrefix(k, strings.ToUpper(j.ProcessName)+"_")
		envs[i] = name + "=" + os.Getenv(k)
	}
	j.execCmd.Env = envs
	j.logger.Debugf("Registered %v env vars", len(envs))

	// Create a new file or overwrite if it exists
	logFile, err := os.Create(fmt.Sprintf("%s/%s.process.jsonl", os.Getenv("TMP_JOB_LOGS_DIR"), j.UUID))
	if err != nil {
		j.logger.Errorf("Failed to create log file: %s", err.Error())
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}
	defer logFile.Close()

	// Redirect stdout and stderr to the log file
	j.execCmd.Stdout = logFile
	j.execCmd.Stderr = logFile

	// Start the command
	err = j.execCmd.Start()
	if err != nil {
		j.logger.Errorf("Failed to start subprocess. Error: %s", err.Error())
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}
	j.PID = fmt.Sprintf("%d", j.execCmd.Process.Pid)
	j.NewStatusUpdate(RUNNING, time.Time{})

	// Check if job was cancelled (Kill() was called) before waiting for process
	select {
	case <-j.ctx.Done():
		return
	default:
	}

	// Wait for the process to finish
	err = j.execCmd.Wait()
	if err != nil {
		if j.CurrentStatus() == DISMISSED {
			return
		} else {
			j.logger.Errorf("Subprocess failure. Error: %s", err.Error())
			j.NewStatusUpdate(FAILED, time.Time{})
			return
		}
	}

	j.logger.Info("Subprocess finished successfully.")
	j.NewStatusUpdate(SUCCESSFUL, time.Time{})
	go j.WriteMetaData()
}

// Kill subprocess
func (j *SubprocessJob) Kill() error {
	j.logger.Info("Received dismiss signal.")
	switch j.CurrentStatus() {
	case SUCCESSFUL, FAILED, DISMISSED:
		// if these jobs have been loaded from previous snapshot they would not have context etc
		return fmt.Errorf("can't call delete on an already completed, failed, or dismissed job")
	}

	j.NewStatusUpdate(DISMISSED, time.Time{})
	// If a dismiss status is updated the job is considered dismissed at this point
	// Close being graceful or not does not matter.

	// Cancel context to signal Run() to exit early if still executing.
	// Close() is safe to call from both here and Run()'s defer because
	// closeOnce guarantees the cleanup body executes exactly once.
	j.ctxCancel()

	go j.Close()
	return nil
}

// Write metadata at the job's metadata location
func (j *SubprocessJob) WriteMetaData() {
	j.logger.Info("Starting metadata writing routine.")
	j.wg.Add(1)
	defer j.wg.Done()
	defer j.logger.Info("Finished metadata writing routine.")

	p := process{j.ProcessID(), j.ProcessVersionID()}
	repoURL := os.Getenv("REPO_URL")

	md := metaData{
		Context:         fmt.Sprintf("%s/blob/main/context.jsonld", repoURL),
		JobID:           j.UUID,
		Process:         p,
		Commands:        j.Cmd,
		GeneratedAtTime: j.UpdateTime,
		StartedAtTime:   j.UpdateTime,
		EndedAtTime:     j.UpdateTime,
	}

	jsonBytes, err := json.Marshal(md)
	if err != nil {
		j.logger.Errorf("Error marshalling metadata to JSON bytes: %s", err.Error())
		return
	}

	metadataDir := os.Getenv("STORAGE_METADATA_PREFIX")
	mdLocation := fmt.Sprintf("%s/%s.json", metadataDir, j.UUID)
	err = utils.WriteToS3(j.StorageSvc, jsonBytes, mdLocation, "application/json", 0)
	if err != nil {
		return
	}
}

func (j *SubprocessJob) RunFinished() {
	// do nothing because for local subprocess jobs decrementing wgRun is handled by Run Function
	// This prevents wgDone being called twice and causing panics
}

// Write final logs, cancelCtx
func (j *SubprocessJob) Close() {
	// closeOnce.Do() ensures this cleanup runs exactly once, even if Close() is called
	// multiple times concurrently.
	//
	// How sync.Once works:
	//   - First caller: acquires internal lock, executes the function, marks done
	//   - Concurrent/subsequent callers: see done flag, return immediately without executing
	j.closeOnce.Do(func() {
		j.logger.Info("Starting closing routine.")
		j.ctxCancel() // Signal Run function to terminate if running

		// // Following is not needed since we are using context to signal job termination
		// if j.execCmd.Process != nil && j.execCmd.ProcessState == nil {
		// 	// Process related cleanups if process state is nil meaning process is still running
		// 	err := j.execCmd.Process.Kill()
		// 	if err != nil {
		// 		j.logger.Errorf("Could not kill process. Error: %s", err.Error())
		// 	}
		// }

		j.DoneChan <- j // At this point job can be safely removed from active jobs

		go func() {
			j.wg.Wait() // wait if other routines like metadata are running
			j.logFile.Close()
			UploadLogsToStorage(j.StorageSvc, j.UUID, j.ProcessName)
			// It is expected that logs will be requested multiple times for a recently finished job
			// so we are waiting for one hour to before deleting the local copy
			// so that we can avoid repetitive request to storage service.
			// If the server shutdown, these files would need to be manually deleted
			time.Sleep(time.Hour)
			DeleteLocalLogs(j.StorageSvc, j.UUID, j.ProcessName)
		}()
	})
}

func (j *SubprocessJob) IMAGE() string {
	return ""
}

func (j *SubprocessJob) UpdateProcessLogs() (err error) {
	return nil
}
