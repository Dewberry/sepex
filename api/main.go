package main

import (
	"app/auth"
	_ "app/docs"
	"app/handlers"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/natefinch/lumberjack"
	log "github.com/sirupsen/logrus"

	echoSwagger "github.com/swaggo/echo-swagger"
)

var (
	envFP          string
	pluginsLoadDir string
	dbPath         string
	port           string
	logFile        string
	authSvc        string
	authLvl        string
)

func init() {
	// The order of precedence as Flag > Environment variable > Default value

	// Manually parse command line arguments to find the -e value since flag.Parse() can't be used
	for i, arg := range os.Args {
		if arg == "-e" && i+1 < len(os.Args) {
			envFP = os.Args[i+1]
			break
		}
	}

	if envFP != "" {
		err := godotenv.Load(envFP)
		if err != nil {
			log.Fatalf("could not read environment file: %s", err.Error())
		}
	}

	// Only variables that are needed at startup and will not be used after startup are available as CLI flags
	flag.StringVar(&envFP, "e", "", "specify the path of the dot env file to load")
	flag.StringVar(&pluginsLoadDir, "pld", resolveValue("PLUGINS_LOAD_DIR", ""), "specify the relative path of the directory to load plugins from")
	flag.StringVar(&port, "p", resolveValue("API_PORT", "5050"), "specify the port to run the api on")
	flag.StringVar(&logFile, "lf", resolveValue("LOG_FILE", "/.data/logs/api.jsonl"), "specify the log file")
	flag.StringVar(&authSvc, "au", resolveValue("AUTH_SERVICE", ""), "specify the auth service")
	flag.StringVar(&authLvl, "al", resolveValue("AUTH_LEVEL", "0"), "specify the authorization striction level")

	flag.Parse()
}

// Checks if there's an environment variable for this configuration,
// if yes, return the env value, if not, return the default value.
func resolveValue(envVar string, defaultValue string) string {
	if value, exists := os.LookupEnv(envVar); exists {
		return value
	}
	return defaultValue
}

// Init logrus logging, returning, lvl and rotating log writer
// that can be used for middleware logging
func initLogger() (log.Level, *lumberjack.Logger) {

	dir := filepath.Dir(logFile)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Println("Could not access log file, Error:", err.Error())
		os.Exit(1)
	}
	file.Close()

	// Set up lumberjack as a logger
	logWriter := &lumberjack.Logger{
		Filename: logFile, // File output location
		MaxSize:  10,      // Maximum file size before rotation (in megabytes)
		MaxAge:   60,      // Maximum number of days to retain old log files
		Compress: true,    // Whether to compress the rotated files
	}

	log.SetOutput(logWriter)
	log.SetFormatter(&log.JSONFormatter{}) // Set formatter to JSON
	log.SetReportCaller(true)              // Enable logging the calling method

	lvl, err := log.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		log.Warnf("Invalid LOG_LEVEL set: %s, defaulting to INFO", os.Getenv("LOG_LEVEL"))
		lvl = log.InfoLevel
	}
	log.SetLevel(lvl)

	return lvl, logWriter
}

const (
	authLevelNone    = 0
	authLevelPartial = 1
	authLevelAll     = 2
)

func applyAuthMiddleware(e *echo.Echo, protected *echo.Group, as auth.AuthStrategy, authLevel int) {
	switch authLevel {
	case authLevelPartial:
		// Apply the Authorize middleware only to protected group
		protected.Use(auth.Authorize(as))
	case authLevelAll:
		// Apply the Authorize middleware to all routes
		e.Use(auth.Authorize(as))
	}
}

func initAuth(e *echo.Echo, protected *echo.Group) int {
	var as auth.AuthStrategy
	var err error

	authLvlInt, err := strconv.Atoi(authLvl)
	if err != nil {
		log.Fatalf("Error converting AUTH_LEVEL to number: %s", err.Error())
	}

	if authLvlInt == 0 {
		log.Warn("No authentication set up.")
		return 0
	} else {
		switch authSvc {
		case "keycloak":
			as, err = auth.NewKeycloakAuthStrategy()
			if err != nil {
				log.Fatalf("Error creating KeyCloak auth service: %s", err.Error())
			}
		default:
			log.Fatal("unsupported auth service provider type")
		}
	}

	applyAuthMiddleware(e, protected, as, authLvlInt)
	return authLvlInt
}

func initPlugins() {
	pluginsDir, exist := os.LookupEnv("PLUGINS_DIR")
	if !exist {
		log.Fatal("env variable PLUGINS_DIR not set")
	}

	deprecatedDir := filepath.Join(pluginsDir, "deprecated")

	// Check if pluginsDir exists
	if _, err := os.Stat(pluginsDir); os.IsNotExist(err) {
		// Create pluginsDir and deprecated directories
		if err := os.MkdirAll(pluginsDir, 0755); err != nil {
			log.Fatal(err)
		}
		if err := os.MkdirAll(deprecatedDir, 0755); err != nil {
			log.Fatal(err)
		}

		// Copy existing plugins from pluginsDir to .data/plugins
		err := copyPlugins(pluginsDir)
		if err != nil {
			log.Fatal(err)
		}
	} else if pluginsLoadDir != "" { // old plugins data exist, yet pluginsLoadDir option specified
		log.Fatal("Plugins Load Directory specified, but old plugins data folder exist. Remove old plugins data first.")
	}
}

func copyPlugins(dstDir string) error {

	if pluginsLoadDir == "" {
		return nil
	}

	if _, err := os.Stat(pluginsLoadDir); os.IsNotExist(err) {
		return fmt.Errorf("specified directory to load plugins from does not exist: %s", pluginsLoadDir)
	}

	// Match only .yml and .yaml files one level down
	ymls, err := filepath.Glob(fmt.Sprintf("%s/*/*.yml", pluginsLoadDir))
	if err != nil {
		return err
	}
	yamls, err := filepath.Glob(fmt.Sprintf("%s/*/*.yaml", pluginsLoadDir))
	if err != nil {
		return err
	}
	allYamls := append(ymls, yamls...)

	for _, srcFile := range allYamls {
		fileName := filepath.Base(srcFile)
		dstFile := filepath.Join(dstDir, strings.TrimSuffix(fileName, filepath.Ext(fileName)), fileName)
		if err := copyFile(srcFile, dstFile); err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	// Ensure the destination directory exists
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}

	return os.WriteFile(dst, input, 0644)
}

// @title SEPEX Server
// @version dev-8.16.23
// @description An OGC compliant process server.

// @contact.name Seth Lawler
// @contact.email slawler@dewberry.com

// @host localhost:5050
// @BasePath /
// @schemes http

// @externalDocs.description  OGC Process API
// @externalDocs.url          https://docs.ogc.org/is/18-062r2/18-062r2.html#toc0

// @externalDocs.description   Schemas
// @externalDocs.url    http://schemas.opengis.net/ogcapi/processes/part1/1.0/openapi/schemas/
func main() {
	initPlugins()

	// Initialize resources
	rh := handlers.NewRESTHander()
	// todo: handle this error: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running
	// todo: all non terminated job statuses should be updated to unknown
	// todo: all logs in the logs directory should be moved to storage

	// Goroutines
	go rh.StatusUpdateRoutine()
	go rh.JobCompletionRoutine()

	// Set server configuration
	e := echo.New()
	e.Static("/public", "public")

	// e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowCredentials: true,
		AllowOrigins:     []string{"*"},
	}))
	e.Renderer = &rh.T

	// Create a group for all routes that need to be protected when AUTH_LEVEL = protected
	pg := e.Group("")
	authLvl := initAuth(e, pg)
	rh.Config.AuthLevel = authLvl

	// Server
	e.GET("/", rh.LandingPage)
	e.GET("/swagger/*", echoSwagger.WrapHandler)
	e.GET("/conformance", rh.Conformance)

	// Processes
	e.GET("/processes", rh.ProcessListHandler)
	e.GET("/processes/:processID", rh.ProcessDescribeHandler)
	pg.POST("/processes/:processID", rh.AddProcessHandler)
	pg.PUT("/processes/:processID", rh.UpdateProcessHandler)
	pg.DELETE("/processes/:processID", rh.DeleteProcessHandler)

	pg.POST("/processes/:processID/execution", rh.Execution)

	// TODO
	// pg.Post("processes/:processID/new, rh.RegisterNewProcess)
	// pg.Delete("processes/:processID", rh.RegisterNewProcess)

	// Jobs
	e.GET("/jobs", rh.ListJobsHandler) // changed for hotfix, should be pg.GET when clients are updated
	e.GET("/jobs/:jobID", rh.JobStatusHandler)
	e.GET("/jobs/:jobID/results", rh.JobResultsHandler)
	e.GET("/jobs/:jobID/logs", rh.JobLogsHandler)
	e.GET("/jobs/:jobID/metadata", rh.JobMetaDataHandler)
	pg.DELETE("/jobs/:jobID", rh.JobDismissHandler)

	// Callbacks
	pg.PUT("/jobs/:jobID/status", rh.JobStatusUpdateHandler)
	// e.POST("/jobs/:jobID/results", rh.JobResultsUpdateHandler)

	_, lw := initLogger()
	fmt.Println("Logging to", logFile)
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Output: lw,
	}))

	// Start server
	go func() {
		log.Info("server starting on port: ", port)
		if err := e.Start(":" + port); err != nil && err != http.ErrServerClosed {
			log.Error("server error : ", err.Error())
			log.Fatal("shutting down the server")
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server with a timeout of 10 seconds.
	// Use a buffered channel to avoid missing signals as recommended for signal.Notify
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	<-quit
	log.Info("gracefully shutting down the server")

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			log.Error(err)
		}
	}()

	// Shutdown the server
	// By default, Docker provides a grace period of 10 seconds with the docker stop command.

	// Kill any running docker containers/subprocess (clean up resources)
	// Send dismiss notice to all cloud jobs
	rh.ActiveJobs.KillAll()
	log.Info("kill command sent to all active jobs")

	// sleep so that Close() routines spawned by KillAll() can finish writing logs, and updating statuses
	// aws batch jobs close() methods take minimum of 5 seconds
	time.Sleep(5 * time.Second)

	if err := rh.DB.Close(); err != nil {
		log.Error(err)
	} else {
		log.Info("closed connection to database")
	}

	time.Sleep(4 * time.Second)

	log.Info("server gracefully shutdown")

}
