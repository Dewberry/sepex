// Package processes register processes from yaml specs
// and provide types and function to interact with these processes
package processes

import (
	"app/controllers"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/gommon/log"
	"gopkg.in/yaml.v3"
)

type Process struct {
	Info    Info      `yaml:"info" json:"info"`
	Host    Host      `yaml:"host" json:"host"`
	Command []string  `yaml:"command" json:"command,omitempty"`
	Config  Config    `yaml:"config" json:"config"`
	Inputs  []Inputs  `yaml:"inputs" json:"inputs"`
	Outputs []Outputs `yaml:"outputs" json:"outputs"`
}

type Link struct {
	Href  string `yaml:"href" json:"href"`
	Rel   string `yaml:"rel,omitempty" json:"rel,omitempty"`
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
}

type Info struct {
	Version            string   `yaml:"version" json:"version"`
	ID                 string   `yaml:"id" json:"id"`
	Title              string   `yaml:"title" json:"title"`
	Description        string   `yaml:"description" json:"description"`
	JobControlOptions  []string `yaml:"jobControlOptions" json:"jobControlOptions"`
	OutputTransmission []string `yaml:"outputTransmission" json:"outputTransmission"`
}

type ValueDefinition struct {
	AnyValue       bool     `yaml:"anyValue" json:"anyValue"`
	PossibleValues []string `yaml:"possibleValues" json:"possibleValues"`
}

type LiteralDataDomain struct {
	DataType        string          `yaml:"dataType" json:"dataType"`
	ValueDefinition ValueDefinition `yaml:"valueDefinition" json:"valueDefinition,omitempty"`
}

type Input struct {
	LiteralDataDomain LiteralDataDomain `yaml:"literalDataDomain" json:"literalDataDomain"`
}

type Inputs struct {
	ID          string `yaml:"id" json:"id"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	Input       Input  `yaml:"input" json:"input"`
	MinOccurs   int    `yaml:"minOccurs" json:"minOccurs"`
	MaxOccurs   int    `yaml:"maxOccurs,omitempty" json:"maxOccurs,omitempty"`
}

type Output struct {
	Formats []string `yaml:"transmissionMode" json:"transmissionMode"`
}

type Outputs struct {
	ID          string `yaml:"id" json:"id"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	Output      Output `yaml:"output" json:"output"`
	InputID     string `yaml:"inputId" json:"inputId,omitempty"`
}

// Resources declares what a process needs from the host it runs on.
//
// CPUs and Memory are advisory: they inform how many jobs the queue runs at
// once, and nothing enforces them at launch. GPUs are enforced, because a GPU
// cannot be subdivided and two jobs sharing one fail rather than slow down.
//
// Field order must stay identical to jobs.Resources, which handlers converts
// to directly.
type Resources struct {
	CPUs   float32 `yaml:"cpus" json:"cpus,omitempty"`
	Memory int     `yaml:"memory" json:"memory,omitempty"`
	GPUs   int     `yaml:"gpus" json:"gpus,omitempty"`
}

type Host struct {
	Type          string `yaml:"type" json:"type"`
	JobDefinition string `yaml:"jobDefinition" json:"jobDefinition,omitempty"`
	JobQueue      string `yaml:"jobQueue" json:"jobQueue,omitempty"`
	Image         string `yaml:"image" json:"image"`
}

type Config struct {
	EnvVars   []string  `yaml:"envVars" json:"envVars,omitempty"`
	Volumes   []string  `yaml:"volumes" json:"volumes,omitempty"`
	Resources Resources `yaml:"maxResources" json:"maxResources,omitempty"`
}

func (p Process) Type() string {
	return p.Host.Type
}

type inpOccurance struct {
	occur    int
	minOccur int
	maxOccur int
}

// jobDefIsPinned reports whether an AWS Batch job definition reference names a
// specific revision, as `name:revision` or as an ARN ending in one. A reference
// without a revision is resolved by Batch to the latest active revision at
// submission, so what it runs can change without the plugin changing.
func jobDefIsPinned(jobDef string) bool {
	name := jobDef
	if i := strings.LastIndex(name, "/"); i != -1 {
		name = name[i+1:] // drop any ARN prefix
	}

	i := strings.LastIndex(name, ":")
	if i == -1 {
		return false
	}

	revision := name[i+1:]
	if revision == "" {
		return false
	}
	for _, r := range revision {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// warnIfUnpinned reports how reproducible a process is without blocking it.
func (p Process) warnIfUnpinned() {
	switch p.Host.Type {
	case "aws-batch":
		if !jobDefIsPinned(p.Host.JobDefinition) {
			log.Warnf(
				"process %s: job definition %q has no revision, so Batch will run whichever revision is latest at submission. Pin it as name:revision for reproducible runs.",
				p.Info.ID, p.Host.JobDefinition,
			)
			// Without a revision there is no single definition to inspect, so
			// there is nothing further to check.
			return
		}

		c, err := controllers.NewAWSBatchController()
		if err != nil {
			log.Warnf("process %s: could not check whether the job definition image is pinned: %v", p.Info.ID, err)
			return
		}

		img, err := c.GetJobDefImage(p.Host.JobDefinition)
		if err != nil {
			log.Warnf("process %s: could not check whether the job definition image is pinned: %v", p.Info.ID, err)
			return
		}

		if !strings.Contains(img, "@sha256:") {
			log.Warnf(
				"process %s: job definition %s uses image %q, which is a moving tag. Register a revision with a digest pinned image for reproducible runs.",
				p.Info.ID, p.Host.JobDefinition, img,
			)
		}

	case "docker":
		if !strings.Contains(p.Host.Image, "@sha256:") {
			log.Warnf(
				"process %s: image %q is a moving tag, so its contents can change without the plugin changing. Pin it as repository@sha256:... for reproducible runs.",
				p.Info.ID, p.Host.Image,
			)
		}

	case "subprocess":
		// A subprocess runs against whatever is installed on the host. There is
		// no image or artifact to pin, so there is nothing to warn about.
	}
}

func (p Process) VerifyInputs(inp map[string]interface{}) error {

	requestInp := make(map[string]*inpOccurance)

	for _, i := range p.Inputs {
		requestInp[i.ID] = &inpOccurance{0, i.MinOccurs, i.MaxOccurs}
	}

	for k, val := range inp {
		o, ok := requestInp[k]
		if ok {
			switch v := val.(type) {
			case []interface{}:
				o.occur = len(v)
			default:
				o.occur = 1
			}
		} else {
			return fmt.Errorf("%s is not a valid input option for this process, use /processes/%s endpoint to get list of input options", k, p.Info.ID)
		}
	}

	for id, oc := range requestInp {
		if (oc.maxOccur > 0 && oc.occur > oc.maxOccur) || (oc.occur < oc.minOccur) {
			return errors.New("Not the correct number of occurance of input: " + id)
		}
	}

	return nil
}

func (p Process) VerifyLocalEnvars() error {
	var missingEnvVars []string
	for _, envVar := range p.Config.EnvVars {
		// check all env vars start with process id
		if !strings.HasPrefix(envVar, strings.ToUpper(p.Info.ID)) {
			return fmt.Errorf("error: env variable %s does not start with %s", envVar, strings.ToUpper(p.Info.ID))
		}
		if os.Getenv(envVar) == "" {
			missingEnvVars = append(missingEnvVars, envVar)
		}
	}
	if len(missingEnvVars) > 0 {
		return fmt.Errorf("error: env variables not found: %v. please restart the server with these in place", missingEnvVars)
	}
	return nil
}

// EnsureLocalVolumes checks if the local volumes exist and creates them if not.
// It validates each volume specification and ensures the host path is a directory.
func (p Process) EnsureLocalVolumes() (err error) {
	for _, volumeSpec := range p.Config.Volumes {
		// Split volume specification into source path and container path (if present)
		parts := strings.Split(volumeSpec, ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid volume specification %q: missing source path", volumeSpec)
		}
		srcPath := strings.TrimSpace(parts[0])
		if srcPath == "" {
			return fmt.Errorf("invalid volume specification %q: empty source path", volumeSpec)
		}

		info, err := os.Stat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.MkdirAll(srcPath, 0755); err != nil {
					return fmt.Errorf("error creating source volume directory %s: %w", srcPath, err)
				}
				continue
			}
			// Handle other errors (e.g., permission issues)
			return fmt.Errorf("error checking source volume directory %s: %w", srcPath, err)
		}

		// Ensure existing path is a directory
		if !info.IsDir() {
			return fmt.Errorf("source volume path %s exists but is not a directory", srcPath)
		}
	}
	return nil
}

// ProcessList describes processes
// This is not a map since ProcessList Handler function wants order
type ProcessList struct {
	List     []Process
	InfoList []Info
}

func (ps *ProcessList) Get(processID string) (Process, int, error) {
	for i, p := range (*ps).List {
		if p.Info.ID == processID {
			return p, i, nil
		}
	}
	return Process{}, 0, errors.New("process not found")
}

func MarshallProcess(f string) (Process, error) {
	var p Process
	data, err := os.ReadFile(f)
	if err != nil {
		return p, err
	}
	err = yaml.Unmarshal(data, &p)
	if err != nil {
		return Process{}, err
	}

	switch p.Host.Type {
	case "aws-batch":
		// The job definition reference is served exactly as the author wrote
		// it. Resolving it here would have frozen an answer that Batch resolves
		// again at submission, and a failed lookup would have dropped the
		// process from the catalog entirely. What a job actually ran is
		// recorded per job, in its metadata, instead.
	case "docker", "subprocess":
		// Set default resources if not specified in config
		if p.Config.Resources.CPUs == 0 {
			p.Config.Resources.CPUs = 1.0
		}
		if p.Config.Resources.Memory == 0 {
			p.Config.Resources.Memory = 512
		}
	}

	return p, nil
}

// Load all processes from yml files in the given directory and subdirectories.
// maxCPUs, maxMemory, and maxGPUs are resource limits for validating docker/subprocess processes.
func LoadProcesses(dir string, maxCPUs float32, maxMemory int, maxGPUs int) (ProcessList, error) {
	var pl ProcessList

	ymls, err := filepath.Glob(fmt.Sprintf("%s/*/*.yml", dir))
	if err != nil {
		return pl, err
	}
	yamls, err := filepath.Glob(fmt.Sprintf("%s/*/*.yaml", dir))
	if err != nil {
		return pl, err
	}
	allYamls := append(ymls, yamls...)
	processes := make([]Process, 0)

	for _, y := range allYamls {
		p, err := MarshallProcess(y)
		if err != nil {
			log.Errorf("could not register process %s Error: %v", filepath.Base(y), err)
			continue
		}
		err = p.Validate(maxCPUs, maxMemory, maxGPUs)
		if err != nil {
			log.Errorf("could not register process %s Error: %v", filepath.Base(y), err.Error())
			continue
		}
		processes = append(processes, p)
	}

	infos := make([]Info, len(processes))
	for i, p := range processes {
		infos[i] = p.Info
	}

	pl.List = processes
	pl.InfoList = infos

	return pl, nil
}

// Validate checks if the Process has all required fields properly set.
// maxCPUs, maxMemory, and maxGPUs are the resource limits for local job
// scheduling. Pass 0 for maxCPUs or maxMemory to skip validating them.
//
// maxGPUs has no such escape: 0 means the host genuinely has no GPUs, which is
// the common case, so it is always meaningful. It only ever produces a warning
// regardless, never a validation failure.
func (p *Process) Validate(maxCPUs float32, maxMemory int, maxGPUs int) error {
	if p.Info.ID == "" {
		return errors.New("process ID is required")
	}
	if p.Info.Title == "" {
		return errors.New("process title is required")
	}
	if p.Info.Version == "" {
		return errors.New("version is required")
	}

	// Validate jobControlOptions
	validJobControlOptions := map[string]bool{
		"sync-execute":  true,
		"async-execute": true,
	}
	for _, option := range p.Info.JobControlOptions {
		if !validJobControlOptions[option] {
			return fmt.Errorf("invalid jobControlOption: %s; must be one of [sync-execute, async-execute]", option)
		}
	}

	// Validate outputTransmission
	validOutputTransmission := map[string]bool{
		"reference": true,
		"value":     true,
	}
	for _, transmission := range p.Info.OutputTransmission {
		if !validOutputTransmission[transmission] {
			return fmt.Errorf("invalid outputTransmission: %s; must be one of [reference, value]", transmission)
		}
	}

	// to do: use CASE: here to do each validation for right hosts

	// Validate Host Type
	if p.Host.Type != "docker" && p.Host.Type != "aws-batch" && p.Host.Type != "subprocess" {
		return errors.New("host type must be 'docker' or 'aws-batch' or 'subprocess'")
	}

	// Validate Container Image (if applicable)
	if p.Host.Type == "docker" && p.Host.Image == "" {
		return errors.New("container image is required for docker host type")
	}

	// Validate AWS data (if applicable)
	if p.Host.Type == "aws-batch" && (p.Host.JobQueue == "" || p.Host.JobDefinition == "") {
		return errors.New("job information is required for aws-batch host type")
	}

	// Reproducibility advice. None of this blocks a process from registering:
	// an unpinned process still runs, it just cannot promise that two runs of
	// the same process version execute the same code.
	p.warnIfUnpinned()

	// Validate Environment Variables available
	if err := p.VerifyLocalEnvars(); err != nil {
		return fmt.Errorf("error: %v", err)
	}

	// Validate Host Volume could be created or exist
	if p.Host.Type == "docker" {
		c, err := controllers.NewDockerController()
		if err != nil {
			return fmt.Errorf("error: %v", err)
		}
		if err := c.EnsureImage(context.TODO(), p.Host.Image, false); err != nil {
			return fmt.Errorf("error: %v", err)
		}

		if err := p.EnsureLocalVolumes(); err != nil {
			return fmt.Errorf("error: %v", err)
		}
	}

	// Validate resource limits for local job types (docker/subprocess)
	if p.Host.Type == "docker" || p.Host.Type == "subprocess" {
		if maxCPUs > 0 && p.Config.Resources.CPUs > maxCPUs {
			return fmt.Errorf("process requires %.2f CPUs but max allowed is %.2f", p.Config.Resources.CPUs, maxCPUs)
		}
		if maxMemory > 0 && p.Config.Resources.Memory > maxMemory {
			return fmt.Errorf("process requires %dMB memory but max allowed is %dMB", p.Config.Resources.Memory, maxMemory)
		}
		if p.Config.Resources.GPUs < 0 {
			return fmt.Errorf("process requires %d GPUs; the value must not be negative", p.Config.Resources.GPUs)
		}

		// GPU availability is deliberately not validated here. The same
		// yaml is meant to load on GPU and non-GPU hosts alike, so a host
		// fact must not drop a process from it -- the same reasoning that
		// leaves aws-batch job definitions unresolved. Jobs that can never run
		// are rejected at submission instead, where the message can be
		// actionable.
		if p.Config.Resources.GPUs > maxGPUs {
			log.Warnf("process %s requires %d GPU(s) but only %d are available on this host; its jobs will be rejected at submission",
				p.Info.ID, p.Config.Resources.GPUs, maxGPUs)
		}

		if p.Host.Type == "subprocess" && p.Config.Resources.GPUs > 0 {
			log.Warnf("process %s declares %d GPU(s), but GPU allocation is not supported for subprocess processes; its jobs will be rejected at submission",
				p.Info.ID, p.Config.Resources.GPUs)
		}
	}

	// Validate Inputs
	for i, input := range p.Inputs {
		if input.ID == "" {
			return fmt.Errorf("input %d: ID is required", i)
		}
	}

	// Validate Outputs
	for i, output := range p.Outputs {
		if output.ID == "" {
			return fmt.Errorf("output %d: ID is required", i)
		}
	}

	return nil
}
