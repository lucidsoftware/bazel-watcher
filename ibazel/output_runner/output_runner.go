// Copyright 2017 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package output_runner

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/bazelbuild/bazel-watcher/ibazel/workspace_finder"
	blaze_query "github.com/bazelbuild/bazel-watcher/third_party/bazel/master/src/main/protobuf"
)

var runOutput = flag.Bool("run_output", false, "Search for commands in Bazel output that match a regex and execute them")
var runOutputInteractive = flag.Bool(
	"run_output_interactive",
	true,
	"Use an interactive prompt when executing commands in Bazel output")

type OutputRunner struct{}

func New() *OutputRunner {
	i := &OutputRunner{}
	return i
}

func (i *OutputRunner) Initialize(info *map[string]string) {}

func (i *OutputRunner) TargetDecider(rule *blaze_query.Rule) {}

func (i *OutputRunner) ChangeDetected(targets []string, changeType string, change string) {}

func (i *OutputRunner) BeforeCommand(targets []string, command string) {}

func (i *OutputRunner) AfterCommand(targets []string, command string, success bool, output *bytes.Buffer) {
	if !*runOutput || output == nil {
		return
	}

	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		line := scanner.Text()
		re := regexp.MustCompile("^(buildozer) '(.*)'(.*)$")
		matches := re.FindStringSubmatch(line)
		if matches != nil && len(matches) >= 3 {
			if *runOutputInteractive {
				if promptCommand(matches[0]) {
					executeCommand(matches[1], matches[2:])
				}
			} else {
				executeCommand(matches[1], matches[2:])
			}
		}
	}
}

func promptCommand(command string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(os.Stderr, "Do you want to execute this command?\n%s\n[y/N]", command)
	text, _ := reader.ReadString('\n')
	text = strings.ToLower(text)
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, "\n")
	if text == "y" {
		return true
	} else {
		return false
	}
}

func executeCommand(command string, args []string) {
	for i, arg := range args {
		args[i] = strings.TrimSpace(arg)
	}
	fmt.Fprintf(os.Stderr, "Executing command: %s\n", command)
	workspaceFinder := &workspace_finder.MainWorkspaceFinder{}
	workspacePath, err := workspaceFinder.FindWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding workspace: %v\n", err)
		os.Exit(5)
	}
	fmt.Fprintf(os.Stderr, "Workspace path: %s\n", workspacePath)

	ctx, _ := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command, args...)
	fmt.Fprintf(os.Stderr, "Executing command: %s %s\n", cmd.Path, strings.Join(cmd.Args, ","))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = workspacePath

	err = cmd.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Command failed: %s %s. Error: %s\n", command, args, err)
	}
}

func (i *OutputRunner) Cleanup() {}
