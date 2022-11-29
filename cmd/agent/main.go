package main

import (
	"fmt"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"os"

	"github.com/kubeshop/testkube-executor-template/pkg/runner"
	"github.com/kubeshop/testkube/pkg/executor/agent"
)

func main() {
	r, err := runner.NewRunner(os.Getenv("DEPENDENCY_MANAGER"))
	if err != nil {
		output.PrintError(os.Stderr, fmt.Errorf("could not initialize runner: %w", err))
		os.Exit(1)
	}
	agent.Run(r, os.Args)
}
