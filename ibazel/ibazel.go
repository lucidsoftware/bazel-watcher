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

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/bazelbuild/bazel-watcher/bazel"
	"github.com/bazelbuild/bazel-watcher/ibazel/command"
	"github.com/bazelbuild/bazel-watcher/ibazel/live_reload"
	"github.com/bazelbuild/bazel-watcher/ibazel/log"
	"github.com/bazelbuild/bazel-watcher/ibazel/output_runner"
	"github.com/bazelbuild/bazel-watcher/ibazel/profiler"
	"github.com/bazelbuild/bazel-watcher/ibazel/workspace_finder"
	"github.com/bazelbuild/bazel-watcher/third_party/bazel/master/src/main/protobuf/blaze_query"
)

var osExit = os.Exit
var bazelNew = bazel.New
var commandDefaultCommand = command.DefaultCommand
var commandNotifyCommand = command.NotifyCommand
var mrunToFiles = flag.Bool("mrunToFiles", false, "Log mrun to file for simpler log reading")

type State string
type runnableCommand func(...string) (*bytes.Buffer, error)
type runnableCommands func([]string, [][]string, int) ([]*bytes.Buffer, error)

const (
	DEBOUNCE_QUERY State = "DEBOUNCE_QUERY"
	QUERY          State = "QUERY"
	WAIT           State = "WAIT"
	DEBOUNCE_RUN   State = "DEBOUNCE_RUN"
	RUN            State = "RUN"
	QUIT           State = "QUIT"
)

const sourceQuery = "kind('source file', deps(set(%s)))"
const buildQuery = "buildfiles(deps(set(%s)))"

type IBazel struct {
	debounceDuration time.Duration

	cmd              command.Command
	cmds             map[string]command.Command
	logFiles         map[string]*os.File
	srcDirToWatch    map[string][]string
	bldDirToWatch    map[string][]string
	prevDir          string
	firstBuildPassed bool
	args             []string
	bazelArgs        []string
	startupArgs      []string

	sigs           chan os.Signal // Signals channel for the current process
	interruptCount int

	workspaceFinder workspace_finder.WorkspaceFinder

	buildFileWatcher  fSNotifyWatcher
	sourceFileWatcher fSNotifyWatcher

	filesWatched map[fSNotifyWatcher]map[string]struct{} // Inner map is a surrogate for a set

	sourceEventHandler *SourceEventHandler
	lifecycleListeners []Lifecycle

	state State
}

func New() (*IBazel, error) {
	i := &IBazel{}
	err := i.setup()
	if err != nil {
		return nil, err
	}

	i.firstBuildPassed = false
	i.debounceDuration = 100 * time.Millisecond
	i.filesWatched = map[fSNotifyWatcher]map[string]struct{}{}
	i.workspaceFinder = &workspace_finder.MainWorkspaceFinder{}

	i.srcDirToWatch = map[string][]string{}
	i.bldDirToWatch = map[string][]string{}

	i.sigs = make(chan os.Signal, 1)
	signal.Notify(i.sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	liveReload := live_reload.New()
	profiler := profiler.New(Version)
	outputRunner := output_runner.New()

	liveReload.AddEventsListener(profiler)

	i.lifecycleListeners = []Lifecycle{
		liveReload,
		profiler,
		outputRunner,
	}

	info, _ := i.getInfo()
	for _, l := range i.lifecycleListeners {
		l.Initialize(info)
	}

	go func() {
		for {
			i.handleSignals()
		}
	}()

	return i, nil
}

func (i *IBazel) handleSignals() {
	// Got an OS signal (SIGINT, SIGTERM, SIGHUP).
	sig := <-i.sigs

	switch sig {
	case syscall.SIGINT:
		for _, cmd := range i.cmds {
			if cmd.IsSubprocessRunning() {
				cmd.Terminate()
			}
		}
		if i.cmd != nil && i.cmd.IsSubprocessRunning() {
			log.NewLine()
			log.Log("Subprocess killed from getting SIGINT (trigger SIGINT again to stop ibazel)")
			i.cmd.Terminate()
		} else {
			osExit(3)
		}
		break
	case syscall.SIGTERM:
		for _, cmd := range i.cmds {
			if cmd.IsSubprocessRunning() {
				cmd.Terminate()
			}
		}
		if i.cmd != nil && i.cmd.IsSubprocessRunning() {
			log.NewLine()
			log.Log("Subprocess killed from getting SIGTERM")
			i.cmd.Terminate()
		}
		osExit(3)
		return
	case syscall.SIGHUP:
		for _, cmd := range i.cmds {
			if cmd.IsSubprocessRunning() {
				cmd.Terminate()
			}
		}
		if i.cmd != nil && i.cmd.IsSubprocessRunning() {
			log.NewLine()
			log.Log("Subprocess killed from getting SIGHUP")
			i.cmd.Terminate()
		}
		osExit(3)
		return
	default:
		log.Fatal("Got a signal that wasn't handled. Please file a bug against bazel-watcher that describes how you did this. This is a big problem.")
	}

	i.interruptCount += 1
	if i.interruptCount > 2 {
		log.NewLine()
		log.Fatal("Exiting from getting SIGINT 3 times")
		osExit(3)
	}
}

func (i *IBazel) newBazel() bazel.Bazel {
	b := bazelNew()
	b.SetStartupArgs(i.startupArgs)
	b.SetArguments(i.bazelArgs)
	return b
}

func (i *IBazel) SetBazelArgs(args []string) {
	i.bazelArgs = args
}

func (i *IBazel) SetStartupArgs(args []string) {
	i.startupArgs = args
}

func (i *IBazel) SetDebounceDuration(debounceDuration time.Duration) {
	i.debounceDuration = debounceDuration
}

func (i *IBazel) Cleanup() {
	i.buildFileWatcher.Close()
	i.sourceFileWatcher.Close()
	for _, l := range i.lifecycleListeners {
		l.Cleanup()
	}
}

func (i *IBazel) targetDecider(target string, rule *blaze_query.Rule) {
	for _, l := range i.lifecycleListeners {
		// TODO: As the name implies, it would be good to use this to make a
		// determination about if future events should be routed to this listener.
		// Why not do it now?
		// Right now I don't track which file is associated with the end target. I
		// just query for a list of all files that are rdeps of any target that is
		// in the list of targets to build/test/run (although run can only have 1).
		// Since I don't have that mapping right now the information doesn't
		// presently exist to implement this properly. Additionally, since querying
		// is currently in the critical path for getting something the user cares
		// about on screen, I'm not sure that it is wise to do this in the first
		// pass. It might be worth triggering the user action, launching their thing
		// and then running a background thread to access the data.
		l.TargetDecider(rule)
	}
}

func (i *IBazel) changeDetected(targets []string, changeType string, change string) {
	for _, l := range i.lifecycleListeners {
		l.ChangeDetected(targets, changeType, change)
	}
}

func (i *IBazel) beforeCommand(targets []string, command string) {
	for _, l := range i.lifecycleListeners {
		l.BeforeCommand(targets, command)
	}
}

func (i *IBazel) afterCommand(targets []string, command string, success bool, output *bytes.Buffer) {
	for _, l := range i.lifecycleListeners {
		l.AfterCommand(targets, command, success, output)
	}
}

func (i *IBazel) setup() error {
	var err error

	// Even though we are going to recreate this when the query happens, create
	// the pointer we will use to refer to the watchers right now.
	i.buildFileWatcher, err = wrapWatcher(fsnotify.NewWatcher())
	if err != nil {
		return err
	}

	i.sourceFileWatcher, err = wrapWatcher(fsnotify.NewWatcher())
	if err != nil {
		return err
	}

	i.sourceEventHandler = NewSourceEventHandler(i.sourceFileWatcher.Watcher())

	return nil
}

// Run the specified target (singular) in the IBazel loop.
func (i *IBazel) Run(target string, args []string) error {
	i.args = args
	return i.loop("run", i.run, []string{target})
}

// Run the specified target (singular) in the IBazel loop.
func (i *IBazel) RunMultiple(args, target []string, debugArgs [][]string) error {
	i.args = args
	argsLength := len(args)
	return i.loopMultiple("run", i.runMultiple, target, debugArgs, argsLength)
}

// Build the specified targets in the IBazel loop.
func (i *IBazel) Build(targets ...string) error {
	return i.loop("build", i.build, targets)
}

// Test the specified targets in the IBazel loop.
func (i *IBazel) Test(targets ...string) error {
	return i.loop("test", i.test, targets)
}

func (i *IBazel) loop(command string, commandToRun runnableCommand, targets []string) error {
	joinedTargets := strings.Join(targets, " ")

	i.state = QUERY
	for {
		i.iteration(command, commandToRun, targets, joinedTargets)
	}

	return nil
}

func (i *IBazel) loopMultiple(command string, commandToRun runnableCommands, targets []string, debugArgs [][]string, argsLength int) error {
	i.state = QUERY
	for {
		i.iterationMultiple(command, commandToRun, targets, debugArgs, argsLength)
	}

	return nil
}

// fsnotify also triggers for file stat and read operations. Explicitly filter the modifying events
// to avoid triggering builds on file accesses (e.g. due to your IDE checking modified status).
const modifyingEvents = fsnotify.Write | fsnotify.Create | fsnotify.Rename | fsnotify.Remove

func (i *IBazel) iteration(command string, commandToRun runnableCommand, targets []string, joinedTargets string) {
	switch i.state {
	case WAIT:
		select {
		case e := <-i.sourceEventHandler.SourceFileEvents:
			if _, ok := i.filesWatched[i.sourceFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				log.Logf("Changed: %q. Rebuilding...", e.Name)
				i.changeDetected(targets, "source", e.Name)
				i.state = DEBOUNCE_RUN
			}
		case e := <-i.buildFileWatcher.Events():
			if _, ok := i.filesWatched[i.buildFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				log.Logf("Build graph changed: %q. Requerying...", e.Name)
				i.changeDetected(targets, "graph", e.Name)
				i.state = DEBOUNCE_QUERY
			}
		}
	case DEBOUNCE_QUERY:
		select {
		case e := <-i.buildFileWatcher.Events():
			if _, ok := i.filesWatched[i.buildFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				i.changeDetected(targets, "graph", e.Name)
			}
			i.state = DEBOUNCE_QUERY
		case <-time.After(i.debounceDuration):
			i.state = QUERY
		}
	case QUERY:
		// Query for which files to watch.
		log.Logf("Querying for files to watch...")
		i.watchFiles(fmt.Sprintf(buildQuery, joinedTargets), i.buildFileWatcher)
		i.watchFiles(fmt.Sprintf(sourceQuery, joinedTargets), i.sourceFileWatcher)
		i.state = RUN
	case DEBOUNCE_RUN:
		select {
		case e := <-i.sourceEventHandler.SourceFileEvents:
			if _, ok := i.filesWatched[i.sourceFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				i.changeDetected(targets, "source", e.Name)
			}
			i.state = DEBOUNCE_RUN
		case <-time.After(i.debounceDuration):
			i.state = RUN
		}
	case RUN:
		log.Logf("%s %s", strings.Title(verb(command)), joinedTargets)
		i.beforeCommand(targets, command)
		outputBuffer, err := commandToRun(targets...)
		i.afterCommand(targets, command, err == nil, outputBuffer)
		i.state = WAIT
	}
}

func (i *IBazel) iterationMultiple(command string, commandToRun runnableCommands, targets []string, debugArgs [][]string, argsLength int) {
	log.Logf("State: %s", i.state)
	switch i.state {
	case WAIT:
		select {
		case e := <-i.sourceEventHandler.SourceFileEvents:
			if _, ok := i.filesWatched[i.sourceFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				log.Logf("\nChanged: %q. Rebuilding...", e.Name)
				i.changeDetected(targets, "source", e.Name)
				i.state = DEBOUNCE_RUN
			}
		case e := <-i.buildFileWatcher.Events():
			if _, ok := i.filesWatched[i.buildFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				log.Logf("\nBuild graph changed: %q. Requerying...", e.Name)
				i.changeDetected(targets, "graph", e.Name)
				i.state = DEBOUNCE_QUERY
			}
		}
	case DEBOUNCE_QUERY:
		select {
		case e := <-i.buildFileWatcher.Events():
			if _, ok := i.filesWatched[i.buildFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				i.changeDetected(targets, "graph", e.Name)
			}
			i.prevDir, _ = filepath.Split(e.Name)
			i.state = DEBOUNCE_QUERY
		case <-time.After(i.debounceDuration):
			i.state = QUERY
		}
	case QUERY:
		// Query for which files to watch.
		log.Logf("Querying for BUILD files...")
		var toQuery []string
		if i.prevDir != "" {
			toQuery := make([]string, len(i.bldDirToWatch[i.prevDir]))
			copy(toQuery, i.bldDirToWatch[i.prevDir])
		}
		//new file added need to rebuild all and add to graphs
		if len(toQuery) == 0 {
			toQuery = targets
		}
		i.watchManyFiles(buildQuery, toQuery, i.buildFileWatcher, &i.bldDirToWatch)
		log.Logf("Querying for source files...")
		i.watchManyFiles(sourceQuery, toQuery, i.sourceFileWatcher, &i.srcDirToWatch)
		i.prevDir = ""
		i.state = RUN
	case DEBOUNCE_RUN:
		select {
		case e := <-i.sourceEventHandler.SourceFileEvents:
			if _, ok := i.filesWatched[i.sourceFileWatcher][e.Name]; ok && e.Op&modifyingEvents != 0 {
				i.changeDetected(targets, "source", e.Name)
			}
			i.prevDir, _ = filepath.Split(e.Name)
			i.state = DEBOUNCE_RUN
		case <-time.After(i.debounceDuration):
			i.state = RUN
		}
	case RUN:
		if i.cmds != nil {
			for _, target := range targets {
				i.cmds[target].BeforeRebuild()
			}
		}

		var torun []string
		if i.prevDir != "" && i.firstBuildPassed {
			torun = i.srcDirToWatch[i.prevDir]
		} else {
			torun = targets
		}
		
		log.Logf("%s %s", strings.Title(verb(command)), strings.Join(torun, " "))
		i.beforeCommand(torun, command)
		outputBuffers, err := commandToRun(torun, debugArgs, argsLength)
		for _, buffer := range outputBuffers {
			i.afterCommand(torun, command, err == nil, buffer)
		}
		i.prevDir = ""
		i.state = WAIT
	}
}

func verb(s string) string {
	switch s {
	case "run":
		return "running"
	case "Run":
		return "Running"
	default:
		return fmt.Sprintf("%sing", s)
	}
}

func (i *IBazel) build(targets ...string) (*bytes.Buffer, error) {
	b := i.newBazel()

	b.Cancel()
	b.WriteToStderr(true)
	b.WriteToStdout(true)
	outputBuffer, err := b.Build(targets...)
	if err != nil {
		log.Errorf("Build error: %v", err)
		return outputBuffer, err
	}
	return outputBuffer, nil
}

func (i *IBazel) test(targets ...string) (*bytes.Buffer, error) {
	b := i.newBazel()

	b.Cancel()
	b.WriteToStderr(true)
	b.WriteToStdout(true)
	outputBuffer, err := b.Test(targets...)
	if err != nil {
		log.Errorf("Build error: %v", err)
		return outputBuffer, err
	}
	return outputBuffer, err
}

func contains(l []string, e string) bool {
	for _, i := range l {
		if i == e {
			return true
		}
	}
	return false
}

func openFileForLogs(fileToOpen string) *os.File {
	if !*mrunToFiles {
		return nil
	}

	reg, err1 := regexp.Compile("[^a-zA-Z0-9-]+")
	if err1 != nil {
		println(err1)
	}
	processedString := reg.ReplaceAllString(fileToOpen, "")
	os.MkdirAll("/tmp/running/", os.ModePerm)
	filename := fmt.Sprintf("/tmp/running/%s.txt", processedString)
	file, err2 := os.OpenFile(filename, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err2 != nil {
		println(err2)
		return nil
	}
	return file
}

func (i *IBazel) setupRun(target string, debugArg []string, argsLength int) command.Command {
	rule, err := i.queryRule(target)
	if err != nil {
		log.Errorf("Error: %v", err)
	}

	i.targetDecider(target, rule)

	commandNotify := false
	for _, attr := range rule.Attribute {
		if *attr.Name == "tags" && *attr.Type == blaze_query.Attribute_STRING_LIST {
			if contains(attr.StringListValue, "ibazel_notify_changes") {
				commandNotify = true
			}
		}
	}

	if commandNotify {
		log.Logf("Launching with notifications")
		return commandNotifyCommand(i.startupArgs, i.bazelArgs, target, i.args)
	} else {
		// argsLength == -1 when the command is `run`
		// no need to modify i.args
		if len(debugArg) > 0 {
			i.args = append(debugArg, i.args[len(i.args)-argsLength:len(i.args)]...)
		} else if argsLength > -1 {
			i.args = i.args[len(i.args)-argsLength:len(i.args)]
		}
		return commandDefaultCommand(i.startupArgs, i.bazelArgs, target, i.args)
	}
}

func (i *IBazel) run(targets ...string) (*bytes.Buffer, error) {
	if i.cmd == nil {
		// If the command is empty, we are in our first pass through the state
		// machine and we need to make a command object.
		i.cmd = i.setupRun(targets[0], []string{}, -1)
		outputBuffer, err := i.cmd.Start(nil)
		if err != nil {
			log.Errorf("Run start failed %v", err)
		}
		return outputBuffer, err
	}

	log.Logf("Notifying of changes")
	outputBuffer := i.cmd.AfterRebuild(nil)
	return outputBuffer, nil
}

func (i *IBazel) runMultiple(targets []string, debugArgs [][]string, argsLength int) ([]*bytes.Buffer, error) {
	var outputBuffers []*bytes.Buffer
	log.Logf("Rebuilding changed targets")
	outputBufferBuild, errBuild := i.build(targets...)
	i.afterCommand(targets, "build", errBuild == nil, outputBufferBuild)
	if errBuild != nil {
		return append(outputBuffers, outputBufferBuild), errBuild
	}
	i.firstBuildPassed = true
	if i.cmds == nil {
		i.cmds = make(map[string]command.Command)
		i.logFiles = make(map[string]*os.File)
		// If the commands are empty, we are in our first pass through the state
		// machine and we need to make command objects.
		for idx, target := range targets {
			i.logFiles[target] = openFileForLogs(target)
			newcommand := i.setupRun(targets[idx], debugArgs[idx], argsLength)
			i.cmds[target] = newcommand
			outputBuffer, err := newcommand.Start(i.logFiles[target])
			outputBuffers = append(outputBuffers, outputBuffer)
			if err != nil {
				log.Logf("Run start failed %v", err)
				return outputBuffers, err
			}
		}
		return outputBuffers, nil
	}
	log.Logf("Notifying of changes")
	for _, target := range targets {
		outputBuffers = append(outputBuffers, i.cmds[target].AfterRebuild(i.logFiles[target]))
	}
	return outputBuffers, nil
}

func (i *IBazel) queryRule(rule string) (*blaze_query.Rule, error) {
	b := i.newBazel()

	res, err := b.CQuery(rule)
	if err != nil {
		log.Errorf("Error running Bazel %v", err)
		i.sigs <- syscall.SIGTERM
		time.Sleep(10 * time.Second)
	}

	for _, target := range res.Results {
		switch *target.Target.Type {
		case blaze_query.Target_RULE:
			return target.Target.Rule, nil
		}
	}

	return nil, errors.New("No information available")
}

func (i *IBazel) getInfo() (*map[string]string, error) {
	b := i.newBazel()

	res, err := b.Info()
	if err != nil {
		log.Errorf("Error getting Bazel info %v", err)
		return nil, err
	}

	return &res, nil
}

func (i *IBazel) queryForSourceFiles(query string) ([]string, error) {
	b := i.newBazel()

	res, err := b.Query(query)
	if err != nil {
		log.Errorf("Bazel query failed: %v", err)
		i.sigs <- syscall.SIGTERM
		time.Sleep(10 * time.Second)
	}

	workspacePath, err := i.workspaceFinder.FindWorkspace()
	if err != nil {
		log.Errorf("Error finding workspace: %v", err)
		i.sigs <- syscall.SIGTERM
		time.Sleep(10 * time.Second)
	}

	toWatch := make([]string, 0, 10000)
	for _, target := range res.Target {
		switch *target.Type {
		case blaze_query.Target_SOURCE_FILE:
			label := *target.SourceFile.Name
			if strings.HasPrefix(label, "@") {
				continue
			}
			if strings.HasPrefix(label, "//external") {
				continue
			}

			label = strings.Replace(strings.TrimPrefix(label, "//"), ":", string(filepath.Separator), 1)
			toWatch = append(toWatch, filepath.Join(workspacePath, label))
			break
		default:
			log.Errorf("%v\n", target)
		}
	}

	return toWatch, nil
}

func (i *IBazel) watchFiles(query string, watcher fSNotifyWatcher) {
	toWatch, err := i.queryForSourceFiles(query)
	if err != nil {
		// If the query fails, just keep watching the same files as before
		return
	}

	filesFound := map[string]struct{}{}
	filesWatched := map[string]struct{}{}
	uniqueDirectories := map[string][]string{}

	i.watcherAdd(query, watcher, toWatch, filesFound, filesWatched, uniqueDirectories)

	i.watcherRemove(uniqueDirectories, watcher, filesWatched)
}

func (i *IBazel) watchManyFiles(query string, targets []string, watcher fSNotifyWatcher, dirStorage *map[string][]string) {
	toWatchByTarget := map[string][]string{}
	filesFound := map[string]struct{}{}
	filesWatched := map[string]struct{}{}
	uniqueDirectories := map[string][]string{}

	for _, target := range targets {
		toWatch, err := i.queryForSourceFiles(fmt.Sprintf(query, target))
		toWatchByTarget[target] = toWatch
		if err != nil {
			// If the query fails, just keep watching the same files as before
			return
		}
	}

	dirWatchedByTarget(toWatchByTarget, targets, *dirStorage)

	for _, target := range targets {
		i.watcherAdd(query, watcher, toWatchByTarget[target], filesFound, filesWatched, uniqueDirectories)
	}

	i.watcherRemove(*dirStorage, watcher, filesWatched)
}

func (i *IBazel) watcherAdd(query string, watcher fSNotifyWatcher, toWatch []string, filesFound map[string]struct{}, filesWatched map[string]struct{}, uniqueDirectories map[string][]string) {
	for _, file := range toWatch {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			filesFound[file] = struct{}{}
		}

		parentDirectory, _ := filepath.Split(file)

		// Add a watch to the file's parent directory, unless it's one we've already watched
		if _, ok := uniqueDirectories[parentDirectory]; ok {
			filesWatched[file] = struct{}{}
		} else {
			err := watcher.Add(parentDirectory)
			if err != nil {
				// Special case for the "defaults package", see https://github.com/bazelbuild/bazel/issues/5533
				if !strings.HasSuffix(filepath.ToSlash(file), "/tools/defaults/BUILD") {
					log.Errorf("Error watching file %q error: %v", file, err)
				}
				continue
			} else {
				filesWatched[file] = struct{}{}
				uniqueDirectories[parentDirectory] = []string{}
			}
		}
	}

	if len(filesFound) == 0 {
		log.Errorf("Didn't find any files to watch from query %s", query)
	}
}

func (i *IBazel) watcherRemove(dirWatched map[string][]string, watcher fSNotifyWatcher, filesWatched map[string]struct{}) {
	for file, _ := range i.filesWatched[watcher] {
		parentDirectory, _ := filepath.Split(file)

		// Remove the watch from the parent directory if it no longer contains any files returned by the latest query
		if _, ok := dirWatched[parentDirectory]; !ok {
			err := watcher.Remove(parentDirectory)
			if err != nil {
				log.Errorf("Error unwatching file %q error: %v\n", file, err)
			}
		}
	}

	i.filesWatched[watcher] = filesWatched
}

func dirWatchedByTarget(toWatchByTarget map[string][]string, targets []string, dirStorage map[string][]string) {
	for dir, _ := range dirStorage {
		for _, target := range targets {
			if idx := containsIdx(dirStorage[dir], target); idx != -1 {
				dirStorage[dir] = deleteIdx(dirStorage[dir], idx)
			}
			if len(dirStorage[dir]) == 0 {
				delete(dirStorage, dir)
			}
			
		}
	}

	for _, target := range targets {
		for _, file := range toWatchByTarget[target] {
			parentDirectory, _ := filepath.Split(file)
			if idx := containsIdx(dirStorage[parentDirectory], target); idx == -1 {
				dirStorage[parentDirectory] = append(dirStorage[parentDirectory], target)
			}
		}
	}
}

// Find string e in string array l
// Return the index if found, -1 if not found
func containsIdx(l []string, e string) int {
	for idx, i := range l {
		if i == e {
			return idx
		}
	}
	return -1
}

// Delete idx element in string array a
func deleteIdx(a []string, idx int) []string {
	a[idx] = a[len(a)-1] // Copy last element to index i.
	a[len(a)-1] = ""   // Erase last element (write zero value).
	a = a[:len(a)-1]
	return a
}
