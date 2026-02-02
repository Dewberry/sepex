package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"app/controllers"

	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/sirupsen/logrus"
)

// RecoverAllJobs rebuilds in-memory state after an API restart.
//
// Design:
// - Query DB once for all non-terminal jobs.
// - For each job:
//   - docker: check container state and recover.
//   - subprocess: not recoverable and will be marked DISMISSED and write server log line.
//   - aws-batch: query AWS for current state; if terminal then it should finalize; if running then add to ActiveJobs.

func RecoverAllJobs(
	db Database,
	storage *s3.S3,
	active *ActiveJobs,
	doneChan chan Job,
	resourcePool *ResourcePool,
	processResources map[string]Resources,
) error {

	records, err := db.GetNonTerminalJobs()
	if err != nil {
		return err
	}

	// Count by host
	var dockerCount, subprocessCount, batchCount int
	for _, r := range records {
		switch r.Host {
		case "docker":
			dockerCount++
		case "subprocess":
			subprocessCount++
		case "aws-batch":
			batchCount++
		}
	}

	log.Infof("Recovery: found %d non-terminal jobs (docker=%d subprocess=%d aws-batch=%d)",
		len(records), dockerCount, subprocessCount, batchCount,
	)

	// Run recoveries in a stable order.
	if err := recoverDockerJobsFromRecords(db, storage, active, doneChan, resourcePool, processResources, records); err != nil {
		return fmt.Errorf("docker recovery failed: %w", err)
	}

	if err := dismissSubprocessJobsFromRecords(db, records); err != nil {
		return fmt.Errorf("subprocess dismissal failed: %w", err)
	}

	if err := recoverAWSBatchJobsFromRecords(db, storage, active, doneChan, records); err != nil {
		return fmt.Errorf("aws-batch recovery failed: %w", err)
	}

	log.Info("Recovery: completed")
	return nil
}

// ---------------------------
// Docker recovery
// ---------------------------

func recoverDockerJobsFromRecords(
	db Database,
	storageSvc *s3.S3,
	activeJobs *ActiveJobs,
	doneChan chan Job,
	resourcePool *ResourcePool,
	processResources map[string]Resources,
	records []JobRecord,
) error {

	dockerCtl, err := controllers.NewDockerController()
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Host != "docker" {
			continue
		}

		if r.Status == ACCEPTED {
			log.Infof("Recovery(docker): ACCEPTED job never started; insufficient data to requeue, marking DISMISSED job=%s", r.JobID)
			_ = db.updateJobRecord(r.JobID, DISMISSED, time.Now())
			continue
		}

		if r.HostJobID == "" {
			if r.Status == RUNNING {
				log.Infof("Recovery(docker): RUNNING job missing container ID, marking LOST job=%s", r.JobID)
				_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			}
			continue
		}

		log.Infof("Recovery(docker): job=%s container=%s status=%s", r.JobID, r.HostJobID, r.Status)

		info, err := dockerCtl.ContainerInfo(context.TODO(), r.HostJobID)
		if err != nil || !info.Exists {
			log.Warnf("Recovery(docker): container missing and will be marked LOST job=%s container=%s", r.JobID, r.HostJobID)
			_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			continue
		}

		job := &DockerJob{
			UUID:        r.JobID,
			ContainerID: r.HostJobID,
			ProcessName: r.ProcessID,
			Status:      RUNNING,
			DB:          db,
			StorageSvc:  storageSvc,
			DoneChan:    doneChan,
			Recovered:   true,
		}

		ctx, cancel := context.WithCancel(context.Background())
		job.ctx = ctx
		job.ctxCancel = cancel

		if err := job.initLogger(); err != nil {
			log.Warnf("Recovery(docker): failed to rebuild in-memory job=%s: %v", r.JobID, err)
			continue
		}

		if resourcePool != nil {
			job.ResourcePool = resourcePool
			if res, ok := processResources[r.ProcessID]; ok {
				job.Resources = res
				resourcePool.ReserveForce(res.CPUs, res.Memory)
			} else {
				log.Warnf("Recovery(docker): process resources not found job=%s process=%s", r.JobID, r.ProcessID)
			}
		}

		// Register in ActiveJobs
		var j Job = job
		activeJobs.Jobs[j.JobID()] = &j
		job.logger.Info("Job recovered after restart. Some features might be missing")

		log.Infof("Recovery(docker): added to ActiveJobs job=%s running=%v exit=%d", r.JobID, info.Running, info.ExitCode)

		if info.Running {
			go recoverRunningContainer(job, dockerCtl)
		} else {
			go recoverExitedContainer(job, info.ExitCode)
		}
	}

	return nil
}

func recoverRunningContainer(j *DockerJob, dockerCtl *controllers.DockerController) {
	defer func() {
		if j.ResourcePool != nil {
			j.ResourcePool.Release(j.Resources.CPUs, j.Resources.Memory)
		}
	}()
	exitCode, err := dockerCtl.ContainerWait(context.TODO(), j.ContainerID)
	finalizeRecoveredDocker(j, exitCode, err)
}

func recoverExitedContainer(j *DockerJob, exitCode int) {
	defer func() {
		if j.ResourcePool != nil {
			j.ResourcePool.Release(j.Resources.CPUs, j.Resources.Memory)
		}
	}()
	finalizeRecoveredDocker(j, int64(exitCode), nil)
}

func finalizeRecoveredDocker(j *DockerJob, exitCode int64, waitErr error) {
	defer j.Close()
	if waitErr != nil {
		j.NewStatusUpdate(FAILED, time.Now())
		return
	}

	if exitCode == 0 {
		j.NewStatusUpdate(SUCCESSFUL, time.Now())
		go j.WriteMetaData()
	} else {
		j.NewStatusUpdate(FAILED, time.Now())
	}
}

// ---------------------------
// Subprocess dismissal
// ---------------------------

// dismissSubprocessJobsFromRecords marks any non-terminal subprocess job as DISMISSED or LOST
// and appends a server log line explaining it was dismissed due to restart/crash.
//
// Subprocess jobs are intentionally not recoverable: after an API restart we cannot
// reliably reconnect to the child process or guarantee its state.
func dismissSubprocessJobsFromRecords(db Database, records []JobRecord) error {
	for _, r := range records {
		if r.Host != "subprocess" {
			continue
		}

		switch r.Status {
		case ACCEPTED:
			log.Infof("Recovery(subprocess): ACCEPTED job never started; insufficient data to requeue, marking DISMISSED job=%s", r.JobID)
			_ = db.updateJobRecord(r.JobID, DISMISSED, time.Now())
		case RUNNING:
			log.Infof("Recovery(subprocess): RUNNING job missing process ID, marking LOST job=%s", r.JobID)
			_ = db.updateJobRecord(r.JobID, LOST, time.Now())
		}

	}
	return nil
}

func appendDismissedDueToRestartLog(jobID string) error {
	dir := os.Getenv("TMP_JOB_LOGS_DIR")
	fp := filepath.Join(dir, fmt.Sprintf("%s.server.jsonl", jobID))

	f, err := os.OpenFile(fp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Match LogEntry fields in jobs.go: Level, Msg, Time
	line := fmt.Sprintf(
		`{"time":"%s","level":"warning","msg":"Job dismissed due to API restart/crash (subprocess jobs are not recoverable)."}%s`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"\n",
	)

	_, err = f.WriteString(line)
	return err
}

// ---------------------------
// AWS Batch recovery
// ---------------------------

func recoverAWSBatchJobsFromRecords(
	db Database,
	storage *s3.S3,
	active *ActiveJobs,
	doneChan chan Job,
	records []JobRecord,
) error {

	batchCtl, err := controllers.NewAWSBatchController(
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
		os.Getenv("AWS_REGION"),
	)
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Host != "aws-batch" || r.HostJobID == "" {
			if r.Host != "aws-batch" {
				continue
			}
			switch r.Status {
			case ACCEPTED:
				log.Warnf("Recovery(aws-batch): ACCEPTED job never started; insufficient data to requeue, marking DISMISSED job=%s", r.JobID)
				_ = db.updateJobRecord(r.JobID, DISMISSED, time.Now())
			case RUNNING:
				log.Warnf("Recovery(aws-batch): RUNNING job missing batch ID, marking LOST job=%s", r.JobID)
				_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			}
			continue
		}

		log.Infof("Recovery(aws-batch): job=%s batch_id=%s status=%s", r.JobID, r.HostJobID, r.Status)

		status, logStream, err := batchCtl.JobMonitor(r.HostJobID)
		if err != nil {
			log.Warnf("Recovery(aws-batch): batch job missing, marking LOST job=%s batch_id=%s", r.JobID, r.HostJobID)
			_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			continue
		}

		j := &AWSBatchJob{
			UUID:          r.JobID,
			AWSBatchID:    r.HostJobID,
			ProcessName:   r.ProcessID,
			Status:        r.Status,
			UpdateTime:    r.LastUpdate,
			logStreamName: logStream,
			batchContext:  batchCtl,
			DB:            db,
			StorageSvc:    storage,
			DoneChan:      doneChan,
		}

		if err := j.initLogger(); err != nil {
			log.Warnf("Recovery(aws-batch): failed to init logger job=%s: %v", r.JobID, err)
			continue
		}

		var job Job = j
		active.Jobs[j.JobID()] = &job
		log.Infof("Recovery(aws-batch): added to ActiveJobs job=%s aws_status=%s", r.JobID, status)

		switch status {
		case "ACCEPTED", "ACCCEPTED":
			j.NewStatusUpdate(ACCEPTED, time.Now())
			// No watcher loop: system expects status updates to come via the status endpoint.

		case "RUNNING":
			j.NewStatusUpdate(RUNNING, time.Now())
			// No watcher loop: system expects status updates to come via the status endpoint.

		case "SUCCEEDED":
			j.NewStatusUpdate(SUCCESSFUL, time.Now())
			go j.Close()

		case "FAILED":
			j.NewStatusUpdate(FAILED, time.Now())
			go j.Close()

		case "DISMISSED":
			j.NewStatusUpdate(DISMISSED, time.Now())
			go j.Close()

		default:
			log.Warnf("Recovery(aws-batch): unhandled aws status=%s job=%s", status, r.JobID)
		}
	}

	return nil
}
