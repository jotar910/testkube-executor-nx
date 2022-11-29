package runner

import (
	"errors"
	"fmt"
	"github.com/caarlos0/env/v6"
	"github.com/joshdk/go-junit"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/secret"
	"github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"strings"
)

type Params struct {
	Endpoint        string `env:"RUNNER_ENDPOINT"`
	AccessKeyID     string `env:"RUNNER_ACCESSKEYID"`
	SecretAccessKey string `env:"RUNNER_SECRETACCESSKEY"`
	Location        string `env:"RUNNER_LOCATION"`
	Token           string `env:"RUNNER_TOKEN"`
	Ssl             bool   `env:"RUNNER_SSL"`
	ScrapperEnabled bool   `env:"RUNNER_SCRAPPERENABLED"`
	GitUsername     string `env:"RUNNER_GITUSERNAME"`
	GitToken        string `env:"RUNNER_GITTOKEN"`
	Datadir         string `env:"RUNNER_DATADIR"`
	NxProject       string `env:"RUNNER_NX_PROJECT"`
	NxCommand       string `env:"RUNNER_NX_COMMAND" envDefault:"e2e"`
}

type NxRunner struct {
	Params     Params
	Fetcher    content.ContentFetcher
	Scraper    scraper.Scraper
	dependency string
}

func NewRunner(dependency string) (*NxRunner, error) {
	logrus.SetLevel(logrus.DebugLevel)

	logrus.Debug("start: reading parameters")
	var params Params
	if err := env.Parse(&params); err != nil {
		return nil, err
	}

	if params.NxProject == "" {
		return nil, errors.New("nx project must be defined (expected RUNNER_NX_PROJECT not to be empty)")
	}

	if params.NxCommand == "" {
		return nil, errors.New("nx command must be defined (expected RUNNER_NX_COMMAND not to be empty)")
	}
	logrus.Debugf("end: reading parameters: %+v", params)

	logrus.Debug("start: preparing fetcher")
	fetcher := content.NewFetcher("")
	logrus.Debug("end: preparing fetcher")

	logrus.Debug("start: preparing scraper")
	scrpr := scraper.NewMinioScraper(
		params.Endpoint,
		params.AccessKeyID,
		params.SecretAccessKey,
		params.Location,
		params.Token,
		params.Ssl,
	)
	logrus.Debug("end: preparing scraper")

	return &NxRunner{
		Params:     params,
		Fetcher:    fetcher,
		Scraper:    scrpr,
		dependency: dependency,
	}, nil
}

func (r *NxRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	logrus.Debug("start: validate execution")
	// make some validation
	err = r.Validate(execution)
	if err != nil {
		return result, err
	}
	logrus.Debug("end: validate execution")

	logrus.Debug("start: checking if data dir exists")
	// check that the datadir exists
	_, err = os.Stat(r.Params.Datadir)
	if errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	logrus.Debug("end: checking if data dir exists")

	logrus.Debug("start: converting executor env variables to os variables")
	// convert executor env variables to os env variables
	for key, value := range execution.Envs {
		if err = os.Setenv(key, value); err != nil {
			return result, fmt.Errorf("setting env var: %w", err)
		}
	}
	logrus.Debug("end: converting executor env variables to os variables")

	runPath := filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.Path)
	if execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.WorkingDir)
	}

	logrus.Debug("start: installing local dependencies")
	// install local dependencies
	if _, err := os.Stat(filepath.Join(runPath, "package.json")); err == nil {
		// be gentle to different cypress versions, run from local npm deps
		out, err := executor.Run(runPath, r.dependency, nil, "install")
		if err != nil {
			return result, fmt.Errorf("%s install error: %w\n\n%s", r.dependency, err, out)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("package.json file not found: %w", err)
	} else {
		return result, fmt.Errorf("checking package.json file: %w", err)
	}
	logrus.Debug("end: installing local dependencies")

	// use `execution.Variables` for variables passed from Test/Execution
	// variables of type "secret" will be automatically decoded
	envManager := secret.NewEnvManager()
	envManager.GetVars(execution.Variables)
	envVars := make([]string, 0, len(execution.Variables))
	for _, value := range execution.Variables {
		envVars = append(envVars, fmt.Sprintf("%s=%s", value.Name, value.Value))
	}

	// prepare args for execution
	args := execution.Args
	if len(envVars) > 0 {
		args = append(args, "--env", strings.Join(envVars, ","))
	}

	command := fmt.Sprintf("./node_modules/.bin/nx run %s --target=%s", r.Params.NxCommand, r.Params.NxProject)
	out, err := executor.Run(runPath, command, envManager, args...)
	if err != nil {
		return result.Err(err), nil
	}

	out = envManager.Obfuscate(out)
	suites, serr := junit.Ingest(out)
	result = MapJunitToExecutionResults(out, suites)

	// scrape artifacts first even if there are errors above
	if r.Params.ScrapperEnabled {
		directories := []string{
			filepath.Join(runPath, "cypress/videos"),
			filepath.Join(runPath, "cypress/screenshots"),
		}
		err := r.Scraper.Scrape(execution.Id, directories)
		if err != nil {
			return result.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
		}
	}

	return result.WithErrors(err, serr), nil

	/*return testkube.ExecutionResult{
		Status: testkube.ExecutionStatusPassed,
		Output: string(envManager.Obfuscate(out)),
	}, nil*/
}

// Validate checks if Execution has valid data in context of Nx executor
// Nx executor runs currently only based on nx project
func (r *NxRunner) Validate(execution testkube.Execution) error {

	if execution.Content == nil {
		return fmt.Errorf("can't find any content to run in execution data: %+v", execution)
	}

	if execution.Content.IsFile() {
		return fmt.Errorf("single file content not supported for nx execution: %+v", execution)
	}

	if execution.Content.Repository == nil {
		return fmt.Errorf("cypress executor handle only repository based tests, but repository is nil")
	}

	if execution.Content.Repository.Branch == "" && execution.Content.Repository.Commit == "" {
		return fmt.Errorf("can't find branch or commit in params, repo:%+v", execution.Content.Repository)
	}

	return nil
}

func MapJunitToExecutionResults(out []byte, suites []junit.Suite) (result testkube.ExecutionResult) {
	status := testkube.PASSED_ExecutionStatus
	result.Status = &status
	result.Output = string(out)
	result.OutputType = "text/plain"

	for _, suite := range suites {
		for _, test := range suite.Tests {

			result.Steps = append(
				result.Steps,
				testkube.ExecutionStepResult{
					Name:     fmt.Sprintf("%s - %s", suite.Name, test.Name),
					Duration: test.Duration.String(),
					Status:   MapStatus(test.Status),
				})
		}

		// TODO parse sub suites recursively

	}

	return result
}

func MapStatus(in junit.Status) (out string) {
	switch string(in) {
	case "passed":
		return string(testkube.PASSED_ExecutionStatus)
	default:
		return string(testkube.FAILED_ExecutionStatus)
	}
}
