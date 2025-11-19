package handlers

import (
	"app/jobs"
	pr "app/processes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// Config holds the configuration settings for the REST API server.
type Config struct {
	// Only settings that are typically environment-specific and can be loaded from
	// external sources like configuration files, environment variables, or remote
	// configuration services, should go here.

	// Read DEV_GUIDE.md to learn about these
	AuthLevel       int
	AdminRoleName   string
	ServiceRoleName string
}

// RESTHandler encapsulates the operational components and dependencies necessary for handling
// RESTful API requests by different handler functions and orchestrating interactions with
// various backend services and resources.
type RESTHandler struct {
	Name         string
	Title        string
	Description  string
	ConformsTo   []string
	T            Template
	StorageSvc   *s3.S3
	DB           jobs.Database
	MessageQueue *jobs.MessageQueue
	ActiveJobs   *jobs.ActiveJobs
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
func NewRESTHander() *RESTHandler {
	apiName, exist := os.LookupEnv("API_NAME")
	if !exist {
		log.Warn("env variable API_NAME not set")
	}

	// working with pointers here so as not to copy large templates, yamls, and ActiveJobs
	config := RESTHandler{
		Name:        apiName,
		Title:       "sepex",
		Description: "ogc process api written in Golang for use with cloud service controllers to manage asynchronous requests",
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
		},
	}

	dbType, exist := os.LookupEnv("DB_SERVICE")
	if !exist {
		log.Fatal("env variable DB_SERVICE not set")
	}

	db, err := jobs.NewDatabase(dbType)
	if err != nil {
		log.Fatalf(err.Error())
	}
	config.DB = db

	// Read all the html templates
	funcMap := template.FuncMap{
		"prettyPrint": prettyPrint, // to pretty print JSONs for results and metadata
		"lower":       strings.ToLower,
		"upper":       strings.ToUpper,
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
		log.Fatalf(err.Error())
	}

	// Setup Active Jobs that will store all jobs currently in process
	ac := jobs.ActiveJobs{}
	ac.Jobs = make(map[string]*jobs.Job)
	config.ActiveJobs = &ac

	config.MessageQueue = &jobs.MessageQueue{
		StatusChan: make(chan jobs.StatusMessage, 500),
		JobDone:    make(chan jobs.Job, 1),
	}

	// Create local logs directory if not exist
	pluginsDir := os.Getenv("PLUGINS_DIR") // We already know this env variable exist because it is being checked in plguinsInit function
	processList, err := pr.LoadProcesses(pluginsDir)
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
