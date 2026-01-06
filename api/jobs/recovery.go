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
			log.Warnf("failed to append restart dismissal log for job %s: %v", r.JobID, err)
		}

		log.Warnf("Dismissed subprocess job %s due to API restart/crash", r.JobID)
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

	// Keep it simple: one JSON line. Your UI already parses JSONL from these files.
	line := fmt.Sprintf(
		`{"time":"%s","level":"WARN","message":"Job dismissed due to API restart/crash (subprocess jobs are not recoverable)."}%s`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"\n",
	)

	_, err = f.WriteString(line)
	return err
}
