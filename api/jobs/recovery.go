package jobs

import (
	"context"
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
