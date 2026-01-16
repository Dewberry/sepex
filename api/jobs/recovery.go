package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"app/controllers"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/labstack/gommon/log"
)

func recoverRunningContainer(j *DockerJob, dockerCtl *controllers.DockerController) {
	exitCode, err := dockerCtl.ContainerWait(context.TODO(), j.ContainerID)
	if err != nil {
		j.NewStatusUpdate(FAILED, time.Now())
		j.Close()
		return
	}

	if exitCode == 0 {
		j.NewStatusUpdate(SUCCESSFUL, time.Now())
	} else {
		j.NewStatusUpdate(FAILED, time.Now())
	}

	j.Close()
}

func recoverExitedContainer(j *DockerJob, exitCode int) {
	if exitCode == 0 {
		j.NewStatusUpdate(SUCCESSFUL, time.Now())
	} else {
		j.NewStatusUpdate(FAILED, time.Now())
	}
	j.Close()
}

func RecoverDockerJobs(
	db Database,
	storageSvc *s3.S3,
	activeJobs *ActiveJobs,
	doneChan chan Job,
) error {

	records, err := db.GetNonTerminalJobs()
	if err != nil {
		return err
	}

	dockerCtl, err := controllers.NewDockerController()
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Host != "docker" || r.HostJobID == "" {
			continue
		}

		log.Info("Recovering docker job ", r.JobID)

		info, err := dockerCtl.ContainerInfo(context.TODO(), r.HostJobID)
		if err != nil || !info.Exists {
			_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			continue
		}

		job, err := NewRecoveredDockerJob(r, db, storageSvc, doneChan)
		if err != nil {
			log.Error("Failed recreating job ", r.JobID)
			continue
		}

		// Register in ActiveJobs
		var j Job = job
		activeJobs.Jobs[j.JobID()] = &j

		if info.Running {
			go recoverRunningContainer(job, dockerCtl)
		} else {
			go recoverExitedContainer(job, info.ExitCode)
		}
	}

	return nil
}

// DismissStaleSubprocessJobs marks any non-terminal subprocess job as DISMISSED
// and writes a server log line explaining it was dismissed due to API restart/crash.
func DismissStaleSubprocessJobs(db Database) error {
	records, err := db.GetNonTerminalJobs()
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Host != "subprocess" {
			continue
		}

		// Mark dismissed
		_ = db.updateJobRecord(r.JobID, DISMISSED, time.Now())

		// Write explanatory line to server logs (best-effort)
		if err := appendDismissedDueToRestartLog(r.JobID); err != nil {
			log.Warn("failed to append restart dismissal log for job ", r.JobID, ": ", err)
		}

		log.Warn("Dismissed subprocess job ", r.JobID, " due to API restart/crash")
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

	line := fmt.Sprintf(
		`{"time":"%s","level":"WARN","msg":"Job dismissed due to API restart/crash (subprocess jobs are not recoverable)."}%s`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"\n",
	)

	_, err = f.WriteString(line)
	return err
}

func RecoverAWSBatchJobs(
	db Database,
	storage *s3.S3,
	active *ActiveJobs,
	doneChan chan Job,
) error {
	records, err := db.GetNonTerminalJobs()
	if err != nil {
		return err
	}

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
			continue
		}

		log.Info("Recovering AWS Batch job ", r.JobID, " (", r.HostJobID, ")")

		status, logStream, err := batchCtl.JobMonitor(r.HostJobID)
		if err != nil {
			log.Error("AWS batch job missing: ", r.HostJobID)
			_ = db.updateJobRecord(r.JobID, LOST, time.Now())
			continue
		}

		// Rebuild the in-memory job
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

		j.initLogger()

		var job Job = j
		active.Jobs[j.JobID()] = &job

		switch status {
		case "RUNNING":
			j.NewStatusUpdate(RUNNING, time.Now())

		case "SUCCEEDED":
			j.NewStatusUpdate(SUCCESSFUL, time.Now())
			go j.Close()

		case "FAILED":
			j.NewStatusUpdate(FAILED, time.Now())
			go j.Close()

		case "DISMISSED":
			j.NewStatusUpdate(DISMISSED, time.Now())
			go j.Close()
		}
	}

	return nil
}

func RecoverAllJobs(
	db Database,
	storage *s3.S3,
	active *ActiveJobs,
	doneChan chan Job,
) error {

	if err := RecoverDockerJobs(db, storage, active, doneChan); err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	if err := DismissStaleSubprocessJobs(db); err != nil {
		return fmt.Errorf("subprocess dismiss: %w", err)
	}

	if err := RecoverAWSBatchJobs(db, storage, active, doneChan); err != nil {
		return fmt.Errorf("aws-batch: %w", err)
	}

	return nil
}
