package handlers

import (
	"app/jobs"
	pr "app/processes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"
)

// Store for templates and a receiver function to render them
type Template struct {
	templates *template.Template
}

// Render the named template with the data
func (t Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

// ResourceLimits holds the maximum resource limits for job scheduling.
// This is read once at startup and shared across the application to ensure
// consistent validation between process registration and job execution.
type ResourceLimits struct {
	MaxCPUs   float32
	MaxMemory int // in MB
	MaxGPUs   int
	// GPUDevices are the specific devices this instance is allowed to hand
	// out. Its length always equals MaxGPUs.
	GPUDevices []jobs.GPUDevice
}

// Config holds the configuration settings for the REST API server.
type Config struct {
	// Only settings that are typically environment-specific and can be loaded from
	// external sources like configuration files, environment variables, or remote
	// configuration services, should go here.

	// Read DEV_GUIDE.md to learn about these
	AuthLevel       int
	AdminRoleName   string
	ServiceRoleName string

	// MaxGroupSize caps how many jobs one job group may ask for.
	MaxGroupSize int

	// Resource limits for local job scheduling (docker/subprocess)
	ResourceLimits *ResourceLimits
}

// RESTHandler encapsulates the operational components and dependencies necessary for handling
// RESTful API requests by different handler functions and orchestrating interactions with
// various backend services and resources.
type RESTHandler struct {
	Name         string
	Title        string
	Description  string
	GitTag       string
	RepoURL      string
	ConformsTo   []string
	T            Template
	StorageSvc   *s3.S3
	DB           jobs.Database
	MessageQueue *jobs.MessageQueue
	ActiveJobs   *jobs.ActiveJobs
	PendingJobs  *jobs.PendingJobs
	ResourcePool *jobs.ResourcePool
	QueueWorker  *jobs.QueueWorker
	ProcessList  *pr.ProcessList
	Config       *Config

	// GroupSubmitter creates the members of accepted job groups in the
	// background, after the request that created the group has returned.
	GroupSubmitter *GroupSubmitter
}

// Pretty print a JSON
func prettyPrint(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// viewFuncMap are the functions the html views may call.
//
// It is a function rather than an inline literal so that the test which parses
// the views can use them too.
func viewFuncMap() template.FuncMap {
	return template.FuncMap{
		"prettyPrint": prettyPrint, // to pretty print JSONs for results and metadata
		"lower":       strings.ToLower,
		"upper":       strings.ToUpper,
		"lastSegment": func(s string) string {
			parts := strings.Split(strings.TrimSuffix(s, "/"), "/")
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
			return s
		},
	}
}

// Initializes resources and return a new handler
// errors are fatal
func NewRESTHander(gitTag string, maxLocalCPUs string, maxLocalMemory string, maxLocalGPUs string, skipGPUVerification string) *RESTHandler {
	apiName, exist := os.LookupEnv("API_NAME")
	if !exist {
		log.Warn("env variable API_NAME not set")
	}

	repoURL, exist := os.LookupEnv("REPO_URL")
	if !exist {
		log.Warn("env variable REPO_URL not set")
	}

	// Calculate resource limits once at startup
	resourceLimits := newResourceLimits(maxLocalCPUs, maxLocalMemory, maxLocalGPUs, skipGPUVerification)

	// working with pointers here so as not to copy large templates, yamls, and ActiveJobs
	config := RESTHandler{
		Name:        apiName,
		Title:       "sepex",
		Description: "SEPEX - Service for Encapsulated Processes Execution. An OGC API - Processes compliant server for executing processes locally or on cloud at scale.",
		GitTag:      gitTag,
		RepoURL:     repoURL,
		ConformsTo: []string{
			"http://schemas.opengis.net/ogcapi/processes/part1/1.0/openapi/schemas/",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/ogc-process-description",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/core",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/json",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/html",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/job-list",
			"http://www.opengis.net/spec/ogcapi-processes-1/1.0/conf/dismiss",
		},
		Config: &Config{
			AdminRoleName:   os.Getenv("AUTH_ADMIN_ROLE"),
			ServiceRoleName: os.Getenv("AUTH_SERVICE_ROLE"),
			ResourceLimits:  resourceLimits,
			MaxGroupSize:    resolveMaxGroupSize(),
		},
	}

	dbType, exist := os.LookupEnv("DB_SERVICE")
	if !exist {
		log.Fatal("env variable DB_SERVICE not set")
	}

	db, err := jobs.NewDatabase(dbType)
	if err != nil {
		log.Fatalf("Failed to create database: %v", err)
	}
	config.DB = db

	// Read all the html templates
	config.T = Template{
		templates: template.Must(template.New("").Funcs(viewFuncMap()).ParseGlob("views/*.html")),
	}

	stType, exist := os.LookupEnv("STORAGE_SERVICE")
	if !exist {
		log.Fatal("env variable STORAGE_SERVICE not set")
	}

	stSvc, err := NewStorageService(stType)
	if err != nil {
		log.Fatal(err)
	}
	config.StorageSvc = stSvc

	// Create local logs directory if not exist
	localLogsDir, exist := os.LookupEnv("TMP_JOB_LOGS_DIR")
	if !exist {
		log.Fatal("env variable TMP_JOB_LOGS_DIR not set")
	}
	err = os.MkdirAll(localLogsDir, 0755)
	if err != nil {
		log.Fatalf("Failed to create logs directory: %v", err)
	}

	// Setup Active Jobs that will store all jobs currently in process
	ac := jobs.ActiveJobs{}
	ac.Jobs = make(map[string]*jobs.Job)
	config.ActiveJobs = &ac

	// Setup Pending Jobs queue for async jobs waiting for resources
	config.PendingJobs = jobs.NewPendingJobs()

	// Setup Resource Pool for tracking CPU/memory availability
	config.ResourcePool = jobs.NewResourcePool(resourceLimits.MaxCPUs, resourceLimits.MaxMemory, resourceLimits.GPUDevices)

	// Setup Queue Worker to process pending jobs
	config.QueueWorker = jobs.NewQueueWorker(config.PendingJobs, config.ResourcePool)

	config.MessageQueue = &jobs.MessageQueue{
		StatusChan: make(chan jobs.StatusMessage, 500),
		JobDone:    make(chan jobs.Job, 1),
	}

	// Create local logs directory if not exist
	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	processList, err := pr.LoadProcesses(pluginsDir, resourceLimits.MaxCPUs, resourceLimits.MaxMemory, resourceLimits.MaxGPUs)
	if err != nil {
		log.Fatal(err)
	}
	config.ProcessList = &processList

	// Takes the handler it will submit through, which is the same value this
	// function returns.
	config.GroupSubmitter = NewGroupSubmitter(&config)

	return &config
}

// This routine sequentially updates status.
// So that order of status updates received is preserved.
func (rh *RESTHandler) StatusUpdateRoutine() {
	for {
		sm := <-rh.MessageQueue.StatusChan
		jobs.ProcessStatusMessageUpdate(sm)
	}
}

func (rh *RESTHandler) JobCompletionRoutine() {
	for {
		j := <-rh.MessageQueue.JobDone
		rh.ActiveJobs.Remove(&j)
	}
}

// Constructor to create storage service based on the type provided
func NewStorageService(providerType string) (*s3.S3, error) {

	switch providerType {
	case "minio":
		region := os.Getenv("MINIO_S3_REGION")
		accessKeyID := os.Getenv("MINIO_ACCESS_KEY_ID")
		secretAccessKey := os.Getenv("MINIO_SECRET_ACCESS_KEY")
		endpoint := os.Getenv("MINIO_S3_ENDPOINT")
		if endpoint == "" {
			return nil, errors.New("`MINIO_S3_ENDPOINT` env var required if STORAGE_SERVICE='minio'")
		}

		sess, err := session.NewSession(&aws.Config{
			Endpoint:         aws.String(endpoint),
			Region:           aws.String(region),
			Credentials:      credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
			S3ForcePathStyle: aws.Bool(true),
		})
		if err != nil {
			return nil, fmt.Errorf("error connecting to minio session: %s", err.Error())
		}
		return s3.New(sess), nil

	case "aws-s3":
		sess, err := session.NewSession()
		if err != nil {
			return nil, fmt.Errorf("error creating s3 session: %s", err.Error())
		}
		return s3.New(sess), nil

	default:
		return nil, fmt.Errorf("unsupported storage provider type")
	}
}

// defaultMaxGroupSize is how many jobs one group may ask for when MAX_GROUP_SIZE is not set.
const defaultMaxGroupSize = 1000

// resolveMaxGroupSize reads MAX_GROUP_SIZE, falling back to the default when it
// is absent or unusable. An unusable value is warned about and defaulted rather
// than being fatal, because the limit only bounds the size of one request: too
// low refuses a large group with a clear message, and nothing is over-committed
// by getting it wrong.
func resolveMaxGroupSize() int {
	value, exist := os.LookupEnv("MAX_GROUP_SIZE")
	if !exist || value == "" {
		return defaultMaxGroupSize
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Warnf("Invalid MAX_GROUP_SIZE value %q: must be a positive integer, using default %d", value, defaultMaxGroupSize)
		return defaultMaxGroupSize
	}
	return parsed
}

// gpuVisibilityHint explains the ways a host with GPUs can still fail to detect
// them, and how to proceed when it cannot. Detection runs in the API process,
// which is frequently itself a container holding only the docker socket, so a
// negative result may be a visibility problem rather than absent hardware.
const gpuVisibilityHint = "If this host has GPUs, ensure the NVIDIA drivers are installed; " +
	"if SEPEX itself is running in a container, that container must also be given GPU visibility " +
	"(`--gpus all`, or compose `deploy.resources.reservations.devices`). Where the API genuinely " +
	"cannot see the GPUs it schedules onto, set SKIP_GPU_VERIFICATION=true (--skip-gpu-verify) " +
	"together with MAX_LOCAL_GPUS to trust that count unverified."

// detectGPUs probes the host for NVIDIA GPUs via nvidia-smi.
//
// The boolean reports whether the probe itself succeeded, which is distinct
// from finding zero devices. When SEPEX runs inside a container with only the
// docker socket mounted it cannot see host GPUs, even though the sibling
// containers it launches can be given them. A failed probe therefore means
// "unknown", not "none", and an explicit MAX_LOCAL_GPUS is trusted without
// being cross-checked against it.
func detectGPUs() ([]jobs.GPUDevice, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,uuid", "--format=csv,noheader").Output()
	if err != nil {
		log.Debugf("GPU detection unavailable: %v", err)
		return nil, false
	}

	var devices []jobs.GPUDevice
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		indexStr, uuid, found := strings.Cut(line, ",")
		if !found {
			log.Warnf("GPU detection: unparseable nvidia-smi row %q", line)
			return nil, false
		}
		index, err := strconv.Atoi(strings.TrimSpace(indexStr))
		if err != nil {
			log.Warnf("GPU detection: unparseable GPU index in nvidia-smi row %q", line)
			return nil, false
		}
		uuid = strings.TrimSpace(uuid)
		if uuid == "" {
			log.Warnf("GPU detection: missing GPU UUID in nvidia-smi row %q", line)
			return nil, false
		}
		devices = append(devices, jobs.GPUDevice{Index: index, UUID: uuid})
	}

	return devices, true
}

// newResourceLimits creates ResourceLimits from the provided values.
// Values come from CLI flags which already have env var fallback via resolveValue().
// Falls back to 80% of system CPUs and 8GB memory if not specified.
//
// CPU and memory misconfiguration is warned about and defaulted, because those
// limits are only advisory inputs to the queue. GPU misconfiguration is fatal:
// GPUs are enforced at container launch, an over-claim would hand the same
// device to two jobs rather than merely slowing one down.
func newResourceLimits(maxLocalCPUsStr string, maxLocalMemoryStr string, maxLocalGPUsStr string, skipGPUVerificationStr string) *ResourceLimits {
	numCPUs := float32(runtime.NumCPU())

	// Default to 80% of system CPUs
	maxCPUs := numCPUs * 0.8
	if maxLocalCPUsStr != "" {
		if parsed, err := strconv.ParseFloat(maxLocalCPUsStr, 32); err == nil {
			maxCPUs = float32(parsed)
		} else {
			log.Warnf("Invalid MAX_LOCAL_CPUS value: %s, using default %.2f", maxLocalCPUsStr, maxCPUs)
		}
	}

	// Default to 8GB
	maxMemory := 8192
	if maxLocalMemoryStr != "" {
		if parsed, err := strconv.Atoi(maxLocalMemoryStr); err == nil {
			maxMemory = parsed
		} else {
			log.Warnf("Invalid MAX_LOCAL_MEMORY_MB value: %s, using default %d", maxLocalMemoryStr, maxMemory)
		}
	}

	skipGPUVerification := false
	if skipGPUVerificationStr != "" {
		parsed, err := strconv.ParseBool(skipGPUVerificationStr)
		if err != nil {
			log.Fatalf("Invalid SKIP_GPU_VERIFICATION value %q: must be a boolean", skipGPUVerificationStr)
		}
		skipGPUVerification = parsed
	}

	maxGPUs, gpuDevices := resolveGPUs(maxLocalGPUsStr, skipGPUVerification)

	log.Infof("ResourceLimits initialized: maxCPUs=%.2f, maxMemory=%dMB, maxGPUs=%d", maxCPUs, maxMemory, maxGPUs)

	return &ResourceLimits{
		MaxCPUs:    maxCPUs,
		MaxMemory:  maxMemory,
		MaxGPUs:    maxGPUs,
		GPUDevices: gpuDevices,
	}
}

// parseMaxGPUs parses MAX_LOCAL_GPUS. Unlike the CPU and memory limits, an
// unusable value is fatal rather than defaulted, because silently falling back
// to 0 would disable every GPU process with no signal.
func parseMaxGPUs(value string) int {
	parsed, err := strconv.Atoi(value)
	switch {
	case err != nil:
		log.Fatalf("Invalid MAX_LOCAL_GPUS value %q: must be an integer", value)
	case parsed < 0:
		log.Fatalf("Invalid MAX_LOCAL_GPUS value %d: must not be negative", parsed)
	}
	return parsed
}

// resolveGPUs determines how many GPUs this instance may schedule and which
// devices those are.
//
// A failed probe is indistinguishable from a host with no GPUs, because on a
// CPU-only machine nvidia-smi is simply absent. Neither case may be overridden
// by MAX_LOCAL_GPUS alone: a device that cannot be enumerated cannot be
// verified, and accepting the value would admit jobs that only fail later,
// when the container runtime rejects a device that does not exist.
//
// skipVerification is the deliberate escape hatch for deployments where the
// API genuinely cannot see the GPUs it schedules onto -- most commonly a
// containerized SEPEX launching sibling containers on the host daemon. It
// makes MAX_LOCAL_GPUS authoritative and unchecked, which is acceptable only
// because the operator has explicitly asserted it.
func resolveGPUs(maxLocalGPUsStr string, skipVerification bool) (int, []jobs.GPUDevice) {
	if skipVerification {
		if maxLocalGPUsStr == "" {
			log.Fatal("SKIP_GPU_VERIFICATION is set but MAX_LOCAL_GPUS is not; there is nothing to infer a GPU count from")
		}

		maxGPUs := parseMaxGPUs(maxLocalGPUsStr)
		if maxGPUs > 0 {
			log.Warnf("GPU verification skipped: trusting MAX_LOCAL_GPUS=%d without enumerating devices. "+
				"Devices are addressed by index rather than UUID, and an incorrect count will surface "+
				"later as containers failing to start.", maxGPUs)
		}

		devices := make([]jobs.GPUDevice, maxGPUs)
		for i := range devices {
			devices[i] = jobs.GPUDevice{Index: i}
		}
		return maxGPUs, devices
	}

	detected, probed := detectGPUs()

	if maxLocalGPUsStr == "" {
		if len(detected) == 0 {
			log.Warnf("GPU detection found 0 GPUs; GPU processes cannot run on this instance. %s", gpuVisibilityHint)
		}
		return len(detected), detected
	}

	maxGPUs := parseMaxGPUs(maxLocalGPUsStr)
	switch {
	case maxGPUs > 0 && !probed:
		log.Fatalf("MAX_LOCAL_GPUS is %d but no GPU could be detected on this host. %s "+
			"Set MAX_LOCAL_GPUS=0 to run without GPUs.", maxGPUs, gpuVisibilityHint)
	case maxGPUs > len(detected):
		log.Fatalf("MAX_LOCAL_GPUS is %d but only %d GPU(s) were detected on this host", maxGPUs, len(detected))
	}
	return maxGPUs, detected[:maxGPUs]
}
