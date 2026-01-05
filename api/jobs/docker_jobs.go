package jobs

import (
	"app/controllers"
	"app/utils"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/sirupsen/logrus"
)

type DockerJob struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	// Used for monitoring meta data and other routines
	wg sync.WaitGroup
	// Used for monitoring running complete for sync jobs
	wgRun sync.WaitGroup
	// closeOnce ensures Close() body executes exactly once
	closeOnce sync.Once

	UUID           string `json:"jobID"`
	ContainerID    string
	Image          string `json:"image"`
	ProcessName    string `json:"processID"`
	ProcessVersion string `json:"processVersion"`
	Submitter      string
	EnvVars        []string
	Volumes        []string `json:"volumes"`
	Cmd            []string `json:"commandOverride"`
	UpdateTime     time.Time
	Status         string `json:"status"`

	logger  *log.Logger
	logFile *os.File

	Resources
	DB           Database
	StorageSvc   *s3.S3
	DoneChan     chan Job
	ResourcePool *ResourcePool
	IsSync       bool
}

func (j *DockerJob) WaitForRunCompletion() {
	j.wgRun.Wait()
}

func (j *DockerJob) JobID() string {
	return j.UUID
}

func (j *DockerJob) ProcessID() string {
	return j.ProcessName
}

func (j *DockerJob) ProcessVersionID() string {
	return j.ProcessVersion
}

func (j *DockerJob) SUBMITTER() string {
	return j.Submitter
}

func (j *DockerJob) CMD() []string {
	return j.Cmd
}

func (j *DockerJob) IMAGE() string {
	return j.Image
}

func (j *DockerJob) GetResources() Resources {
	return j.Resources
}

// Update container logs
func (j *DockerJob) UpdateProcessLogs() (err error) {
	// If old status is one of the terminated status, close has already been called and container logs fetched, container killed
	switch j.Status {
	case SUCCESSFUL, DISMISSED, FAILED:
		return
	}

	j.logger.Debug("Updating container logss")
	containerLogs, err := j.fetchContainerLogs()
	if err != nil {
		j.logger.Error(err.Error())
		return
	}

	if len(containerLogs) == 0 || containerLogs == nil {
		return
	}

	// Create a new file or overwrite if it exists
	file, err := os.Create(fmt.Sprintf("%s/%s.process.jsonl", os.Getenv("TMP_JOB_LOGS_DIR"), j.UUID))
	if err != nil {
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	for i, line := range containerLogs {
		if i != len(containerLogs)-1 {
			_, err = writer.WriteString(line + "\n")
		} else {
			_, err = writer.WriteString(line)
		}
	}

	return
}

func (j *DockerJob) LogMessage(m string, level log.Level) {
	switch level {
	// case 0:
	// 	j.logger.Panic(m)
	// case 1:
	// 	j.logger.Fatal(m)
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

func (j *DockerJob) LastUpdate() time.Time {
	return j.UpdateTime
}

func (j *DockerJob) NewStatusUpdate(status string, updateTime time.Time) {

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

func (j *DockerJob) CurrentStatus() string {
	return j.Status
}

func (j *DockerJob) ProviderID() string {
	return j.ContainerID
}

func (j *DockerJob) Equals(job Job) bool {
	switch jj := job.(type) {
	case *DockerJob:
		return j.ctx == jj.ctx
	default:
		return false
	}
}

func (j *DockerJob) initLogger() error {
	// Create a place holder file for container logs
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

func (j *DockerJob) Create() error {
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
	j.logger.Info("Container Commands: ", j.CMD())

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

func (j *DockerJob) IsSyncJob() bool {
	return j.IsSync
}

func (j *DockerJob) Run() {
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

	c, err := controllers.NewDockerController()
	if err != nil {
		j.logger.Errorf("Failed creating NewDockerController. Error: %s", err.Error())
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}

	err = c.EnsureImage(j.ctx, j.Image, false)
	if err != nil {
		j.logger.Infof("Could not ensure image %s available", j.Image)
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}

	// get environment variables
	envs := make([]string, len(j.EnvVars))
	for i, k := range j.EnvVars {
		name := strings.TrimPrefix(k, strings.ToUpper(j.ProcessName)+"_")
		envs[i] = name + "=" + os.Getenv(k)
	}
	j.logger.Debugf("Registered %v env vars", len(envs))

	resources := controllers.DockerResources{}
	resources.NanoCPUs = int64(j.Resources.CPUs * 1e9)         // Docker controller needs cpu in nano ints
	resources.Memory = int64(j.Resources.Memory * 1024 * 1024) // Docker controller needs memory in bytes

	// although we have already checked if image is available at the time of process init, we are doing it again just to be explicit
	err = c.EnsureImage(j.ctx, j.Image, false)
	if err != nil {
		j.logger.Infof("Could not ensure image %s available", j.Image)
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}

	// start container
	containerID, err := c.ContainerRun(j.ctx, j.Image, j.Cmd, j.Volumes, envs, resources)
	if err != nil {
		j.logger.Errorf("Failed to run container. Error: %s", err.Error())
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}
	j.NewStatusUpdate(RUNNING, time.Time{})

	j.ContainerID = containerID

	// Check if job was cancelled (Kill() was called) before waiting for container
	select {
	case <-j.ctx.Done():
		return
	default:
	}

	// wait for process to finish
	exitCode, err := c.ContainerWait(j.ctx, j.ContainerID)
	if err != nil {
		// to do: check what would happen if container exited because of dismiss signal and hanlde it similar to subprocess_job
		j.logger.Errorf("Failed waiting for container to finish. Error: %s", err.Error())
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}

	if exitCode != 0 {
		j.logger.Errorf("Container failure, exit code: %d", exitCode)
		j.NewStatusUpdate(FAILED, time.Time{})
		return
	}

	j.logger.Info("Container process finished successfully.")
	j.NewStatusUpdate(SUCCESSFUL, time.Time{})
	go j.WriteMetaData()
}

// kill local container
func (j *DockerJob) Kill() error {
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
func (j *DockerJob) WriteMetaData() {
	j.logger.Info("Starting metadata writing routine.")
	j.wg.Add(1)
	defer j.wg.Done()
	defer j.logger.Info("Finished metadata writing routine.")

	c, err := controllers.NewDockerController()
	if err != nil {
		j.logger.Errorf("Could not create controller. Error: %s", err.Error())
	}

	p := process{j.ProcessID(), j.ProcessVersionID()}
	imageDigest, err := c.GetImageDigest(j.IMAGE()) // what if image is update between start of job and this call?
	if err != nil {
		j.logger.Errorf("Error getting Image Digest: %s", err.Error())
		return
	}

	i := image{j.IMAGE(), imageDigest}

	g, s, e, err := c.GetJobTimes(j.ContainerID)
	if err != nil {
		j.logger.Errorf("Error getting job times: %s", err.Error())
		return
	}

	repoURL := os.Getenv("REPO_URL")

	md := metaData{
		Context:         fmt.Sprintf("%s/blob/main/context.jsonld", repoURL),
		JobID:           j.UUID,
		Process:         p,
		Image:           i,
		Commands:        j.Cmd,
		GeneratedAtTime: g,
		StartedAtTime:   s,
		EndedAtTime:     e,
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

// func (j *DockerJob) WriteResults(data []byte) (err error) {
// 	j.logger.Info("Starting results writing routine.")
// 	defer j.logger.Info("Finished results writing routine.")

// 	resultsDir := os.Getenv("STORAGE_RESULTS_PREFIX")
// 	resultsLocation := fmt.Sprintf("%s/%s.json", resultsDir, j.UUID)
// 	fmt.Println(resultsLocation)
// 	err = utils.WriteToS3(j.StorageSvc, data, resultsLocation, "application/json", 0)
// 	if err != nil {
// 		j.logger.Info(fmt.Sprintf("error writing results to storage: %v", err.Error()))
// 	}
// 	return
// }

func (j *DockerJob) fetchContainerLogs() ([]string, error) {
	c, err := controllers.NewDockerController()
	if err != nil {
		return nil, fmt.Errorf("could not create controller to fetch container logs")
	}
	containerLogs, err := c.ContainerLog(context.TODO(), j.ContainerID)
	if err != nil {
		return nil, fmt.Errorf("could not fetch container logs")
	}
	return containerLogs, nil
}

func (j *DockerJob) RunFinished() {
	// do nothing because for local docker jobs decrementing wgRun is handeled by Run Fucntion
	// This prevents wgDone being called twice and causing panics
}

// Write final logs, cancelCtx
func (j *DockerJob) Close() {
	// closeOnce.Do() ensures this cleanup runs exactly once, even if Close() is called
	// multiple times concurrently. This allows for easier development.
	//
	// How sync.Once works:
	//   - First caller: acquires internal lock, executes the function, marks done
	//   - Concurrent/subsequent callers: see done flag, return immediately without executing
	j.closeOnce.Do(func() {
		j.logger.Info("Starting closing routine.")
		j.ctxCancel() // Signal Run function to terminate if running

		if j.ContainerID != "" { // Container related cleanups if container exists
			c, err := controllers.NewDockerController()
			if err != nil {
				j.logger.Errorf("Could not create controller. Error: %s", err.Error())
			} else {
				containerLogs, err := c.ContainerLog(context.TODO(), j.ContainerID)
				if err != nil {
					j.logger.Errorf("Could not fetch container logs. Error: %s", err.Error())
				}

				file, err := os.Create(fmt.Sprintf("%s/%s.process.jsonl", os.Getenv("TMP_JOB_LOGS_DIR"), j.UUID))
				if err != nil {
					j.logger.Errorf("Could not create process logs file. Error: %s", err.Error())
					return
				}

				writer := bufio.NewWriter(file)

				for i, line := range containerLogs {
					if i != len(containerLogs)-1 {
						_, err = writer.WriteString(line + "\n")
					} else {
						_, err = writer.WriteString(line)
					}
					if err != nil {
						j.logger.Errorf("Could not write log %s to file.", line)
					}
				}

				writer.Flush()
				file.Close()

				err = c.ContainerRemove(context.TODO(), j.ContainerID)
				if err != nil {
					j.logger.Errorf("Could not remove container. Error: %s", err.Error())
				}
			}
		}
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
