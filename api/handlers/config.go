package handlers

import (
	"app/jobs"
	pr "app/processes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/template"

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
}

// Pretty print a JSON
func prettyPrint(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// Initializes resources and return a new handler
// errors are fatal
func NewRESTHander(gitTag string, maxLocalCPUs string, maxLocalMemory string) *RESTHandler {
	apiName, exist := os.LookupEnv("API_NAME")
	if !exist {
		log.Warn("env variable API_NAME not set")
	}

	repoURL, exist := os.LookupEnv("REPO_URL")
	if !exist {
		log.Warn("env variable REPO_URL not set")
	}

	// Calculate resource limits once at startup
	resourceLimits := newResourceLimits(maxLocalCPUs, maxLocalMemory)

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
	funcMap := template.FuncMap{
		"prettyPrint":   prettyPrint, // to pretty print JSONs for results and metadata
		"lower":         strings.ToLower,
		"upper":         strings.ToUpper,
		"lastSegment": func(s string) string {
			parts := strings.Split(strings.TrimSuffix(s, "/"), "/")
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
			return s
		},
	}

	config.T = Template{
		templates: template.Must(template.New("").Funcs(funcMap).ParseGlob("views/*.html")),
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
	config.ResourcePool = jobs.NewResourcePool(resourceLimits.MaxCPUs, resourceLimits.MaxMemory)

	// Setup Queue Worker to process pending jobs
	config.QueueWorker = jobs.NewQueueWorker(config.PendingJobs, config.ResourcePool)

	config.MessageQueue = &jobs.MessageQueue{
		StatusChan: make(chan jobs.StatusMessage, 500),
		JobDone:    make(chan jobs.Job, 1),
	}

	// Create local logs directory if not exist
	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	processList, err := pr.LoadProcesses(pluginsDir, resourceLimits.MaxCPUs, resourceLimits.MaxMemory)
	if err != nil {
		log.Fatal(err)
	}
	config.ProcessList = &processList

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
		region := os.Getenv("AWS_REGION")
		accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
		secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String(region),
			Credentials: credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
		})
		if err != nil {
			return nil, fmt.Errorf("error creating s3 session: %s", err.Error())
		}
		return s3.New(sess), nil

	default:
		return nil, fmt.Errorf("unsupported storage provider type")
	}
}

// newResourceLimits creates ResourceLimits from the provided values.
// Values come from CLI flags which already have env var fallback via resolveValue().
// Falls back to 80% of system CPUs and 8GB memory if not specified.
func newResourceLimits(maxLocalCPUsStr string, maxLocalMemoryStr string) *ResourceLimits {
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

	log.Infof("ResourceLimits initialized: maxCPUs=%.2f, maxMemory=%dMB", maxCPUs, maxMemory)

	return &ResourceLimits{
		MaxCPUs:   maxCPUs,
		MaxMemory: maxMemory,
	}
}
