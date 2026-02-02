package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/batch"
)

// Describe Job Definition
type JobDefinitionInfo struct {
	VCPUs  float32
	Memory int
	Image  string
}

type AWSBatchController struct {
	client *batch.Batch
}

// Get job def info from batch
func (c *AWSBatchController) GetJobDefInfo(jobDef string) (JobDefinitionInfo, error) {

	var jdi JobDefinitionInfo
	resp, err := c.client.DescribeJobDefinitions(&batch.DescribeJobDefinitionsInput{
		JobDefinitions: []*string{aws.String(jobDef)},
	})

	if err != nil {
		return jdi, err
	}

	// Check if any job definitions were returned
	if len(resp.JobDefinitions) != 1 {
		return jdi, fmt.Errorf("did not get an exact match for job definitions")
	}

	// Retrieve the Image URI from the first job definition in the response
	jdi.Image = aws.StringValue(resp.JobDefinitions[0].ContainerProperties.Image)
	resourceRequirements := resp.JobDefinitions[0].ContainerProperties.ResourceRequirements

	// Extract vCPU and memory requirements
	f64, err := strconv.ParseFloat(getResourceRequirement(resourceRequirements, "VCPU"), 32)
	if err != nil {
		return jdi, fmt.Errorf("could not parse vCPU reqruiement")
	}
	jdi.VCPUs = float32(f64)
	jdi.Memory, err = strconv.Atoi(getResourceRequirement(resourceRequirements, "MEMORY"))
	if err != nil {
		return jdi, fmt.Errorf("could not parse memory requirement")
	}

	return jdi, nil
}

// Helper function to extract the resource requirement value
func getResourceRequirement(resourceRequirements []*batch.ResourceRequirement, name string) string {
	for _, requirement := range resourceRequirements {
		if *requirement.Type == name {
			return *requirement.Value
		}
	}
	return ""
}

func NewAWSBatchController(accessKey, secretAccessKey, region string) (*AWSBatchController, error) {
	sess, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentialsFromCreds(credentials.Value{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretAccessKey,
		}),
		Region: aws.String(region)},
	)
	if err != nil {
		return nil, err
	}

	return &AWSBatchController{batch.New(sess)}, nil
}

// returns the job id and an error
func (c *AWSBatchController) JobCreate(ctx context.Context,
	jobDef, jobName, jobQueue string, commandOverride []string,
	envVars map[string]string) (string, error) {

	envs := make([]*batch.KeyValuePair, len(envVars))
	var i int
	for k, v := range envVars {
		envs[i] = &batch.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
		i++
	}

	overrides := &batch.ContainerOverrides{
		Command:     aws.StringSlice(commandOverride),
		Environment: envs,
	}

	input := &batch.SubmitJobInput{
		JobDefinition:      aws.String(jobDef),
		JobName:            aws.String(jobName),
		JobQueue:           aws.String(jobQueue),
		ContainerOverrides: overrides,
	}

	output, err := c.client.SubmitJobWithContext(ctx, input)
	if err != nil {
		return "", err
	}

	return aws.StringValue(output.JobId), nil
}

// Get current status of the job from Batch and formats it according to OGC Specs, also get LogStreamName
func (c *AWSBatchController) JobMonitor(batchID string) (string, string, error) {
	input := &batch.DescribeJobsInput{Jobs: aws.StringSlice([]string{batchID})}
	output, err := c.client.DescribeJobs(input)
	if err != nil {
		return "", "", err
	}
	if len(output.Jobs) == 0 {
		return "", "", fmt.Errorf("no such job: %s", batchID)
	}

	status := aws.StringValue(output.Jobs[0].Status)
	lsn := aws.StringValue(output.Jobs[0].Container.LogStreamName)

	switch status {
	case "FAILED":
		reason := aws.StringValue(output.Jobs[0].StatusReason)
		// Non-standard reason used here to facilitate ogc implementation
		if reason == "DISMISSED" {
			return reason, lsn, nil
		} else {
			return status, lsn, nil
		}
	case "SUBMITTED":
		return "ACCEPTED", lsn, nil
	case "PENDING":
		return "ACCEPTED", lsn, nil
	case "RUNNABLE":
		return "ACCEPTED", lsn, nil
	case "STARTING":
		return "RUNNING", lsn, nil
	case "RUNNING":
		return status, lsn, nil
	case "SUCCEEDED":
		return status, lsn, nil

	default:
		return "", lsn, fmt.Errorf("unrecognized status  %s", status)
	}
}

// combines JobTerminate and JobCancel by managing calls for you based on job status
func (c *AWSBatchController) JobKill(jobID string) (string, error) {
	input := &batch.DescribeJobsInput{Jobs: aws.StringSlice([]string{jobID})}

	output, err := c.client.DescribeJobs(input)
	if err != nil {
		return "", err
	}

	if len(output.Jobs) == 0 {
		return "", fmt.Errorf("no such job: %s", jobID)
	}

	if len(output.Jobs) > 1 {
		return "", fmt.Errorf("more than one job found for %s", jobID)
	}

	status := aws.StringValue(output.Jobs[0].Status)
	switch status {
	case "SUBMITTED", "PENDING", "RUNNABLE":
		output, err := c.JobCancel(jobID, "DISMISSED")
		if err != nil {
			return "", err
		}
		return output, nil

	case "STARTING", "RUNNING":
		output, err := c.JobTerminate(jobID, "DISMISSED")
		if err != nil {
			return "", err
		}
		return output, nil

	case "FAILED", "SUCCEEDED":
		return "", nil
	}

	// Add some mechanism to clean up s3 if needed

	return "", fmt.Errorf("unknown status for job %s: %s", jobID, status)
}

// for jobs with the following statuses: "STARTING", "jobs.RUNNING"
func (c *AWSBatchController) JobTerminate(jobID, reason string) (string, error) {
	input := &batch.TerminateJobInput{
		JobId:  aws.String(jobID),
		Reason: aws.String(reason),
	}

	output, err := c.client.TerminateJob(input)
	if err != nil {
		return "", err
	}

	return output.String(), nil
}

// for jobs with the following statuses: "SUBMITTED", "PENDING", "RUNNABLE"
func (c *AWSBatchController) JobCancel(jobID, reason string) (string, error) {
	input := &batch.CancelJobInput{
		JobId:  aws.String(jobID),
		Reason: aws.String(reason),
	}

	output, err := c.client.CancelJob(input)
	if err != nil {
		return "", err
	}

	return output.String(), nil
}

// Get Image URI from Job Definition
func (c *AWSBatchController) GetImageURI(jobDef string) (string, error) {

	resp, err := c.client.DescribeJobDefinitions(&batch.DescribeJobDefinitionsInput{
		JobDefinitions: []*string{aws.String(jobDef)},
	})

	if err != nil {
		return "", err
	}

	// Check if any job definitions were returned
	if len(resp.JobDefinitions) != 1 {
		return "", fmt.Errorf("did not get an exact match for job definitions")
	}

	// Retrieve the Image URI from the first job definition in the response
	imageURI := aws.StringValue(resp.JobDefinitions[0].ContainerProperties.Image)

	return imageURI, nil
}

// Get job execution times
func (c *AWSBatchController) GetJobTimes(batchID string) (cp time.Time, cr time.Time, st time.Time, err error) {

	describeJobsInput := &batch.DescribeJobsInput{
		Jobs: []*string{aws.String(batchID)},
	}

	describeJobsOutput, err := c.client.DescribeJobs(describeJobsInput)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, fmt.Errorf("error describing jobs: %s", err)
	}

	if len(describeJobsOutput.Jobs) > 0 {
		job := describeJobsOutput.Jobs[0] // Assuming only one job is returned

		// Extract createdAt, startedAt, and completedAt times
		if job.CreatedAt != nil && job.StartedAt != nil && job.StoppedAt != nil {
			cr = time.UnixMilli(*job.CreatedAt)
			st = time.UnixMilli(*job.StartedAt)
			cp = time.UnixMilli(*job.StoppedAt)
		} else {
			return time.Time{}, time.Time{}, time.Time{}, fmt.Errorf("one of the job time value is nil")
		}
	} else {
		return time.Time{}, time.Time{}, time.Time{}, fmt.Errorf("no job information found")
	}

	return cr, st, cp, nil
}
