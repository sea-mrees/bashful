package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/lunixbochs/vtclean"
	color "github.com/mgutz/ansi"
	"github.com/tj/go-spin"
	terminal "github.com/wayneashleyberry/terminal-dimensions"
)

var (
	// spinner generates the spin icon character in front of running tasks
	spinner = spin.New()

	// nextDisplayIdx is the next available screen row to use based off of the task / sub-task order.
	nextDisplayIdx = 0

	// lineDefaultTemplate is the string template used to display the status values of a single task with no children
	lineDefaultTemplate, _ = template.New("default line").Parse(` {{.Status}}  ` + color.Reset + ` {{printf "%1s" .Prefix}} {{printf "%-25s" .Title}} {{.Msg}}{{.Split}}{{.Eta}}`)

	// lineParallelTemplate is the string template used to display the status values of a task that is the child of another task
	lineParallelTemplate, _ = template.New("parallel line").Parse(` {{.Status}}  ` + color.Reset + ` {{printf "%1s" .Prefix}} ├─ {{printf "%-25s" .Title}} {{.Msg}}{{.Split}}{{.Eta}}`)

	// lineLastParallelTemplate is the string template used to display the status values of a task that is the LAST child of another task
	lineLastParallelTemplate, _ = template.New("last parallel line").Parse(` {{.Status}}  ` + color.Reset + ` {{printf "%1s" .Prefix}} └─ {{printf "%-25s" .Title}} {{.Msg}}{{.Split}}{{.Eta}}`)
)

// TaskStats is a global struct keeping track of the number of running tasks, failed tasks, completed tasks, and total tasks
var TaskStats struct {
	// runningCmds indicates the number of actively running tasks
	runningCmds int

	// completedTasks indicates the number of tasks that have finished execution (regardless of the return code value)
	completedTasks int

	// totalFailedTasks indicates the number of tasks that have a non-zero return code
	totalFailedTasks int

	// totalTasks is the number of tasks that is expected to be run based on the user configuration
	totalTasks int
}

// Task is a runtime object derived from the TaskConfig (parsed from the user yaml) and contains everything needed to execute, track, and display the task.
type Task struct {
	// Config is the user-defined values parsed from the run yaml
	Config TaskConfig

	// Display represents all non-config items that control how the task line should be printed to the screen
	Display TaskDisplay

	// Command represents all non-config items used to execute and track task progress
	Command TaskCommand

	// LogChan is a channel with event log items written to the temporary logfile
	LogChan chan LogItem

	// LogFile is the temporary log file where all formatted stdout/stderr events are recorded
	LogFile *os.File

	// ErrorBuffer contains all stderr lines generated from the executed command (used to generate the task report)
	ErrorBuffer *bytes.Buffer

	// Children is a list of all sub-tasks that should be run concurrently
	Children []*Task

	// lastStartedTask is the index of the last child task that was started
	lastStartedTask int

	// resultChan is a channel where all raw command events are queued to
	resultChan chan CmdEvent

	// waiter is a synchronization object which returns when all child task command executions have been completed
	waiter sync.WaitGroup

	// status is the last known status value that represents the entire list of child commands
	status CommandStatus

	// failedTasks is a list of tasks with a non-zero return value
	failedTasks []*Task
}

// TaskDisplay represents all non-config items that control how the task line should be printed to the screen
type TaskDisplay struct {
	// Template is the single-line string template that should be used to display the status of a single task
	Template *template.Template

	// Index is the row within a screen frame to print the task template
	Index int

	// Values holds all template values that represent the task status
	Values LineInfo
}

// TaskCommand represents all non-config items used to execute and track task progress
type TaskCommand struct {
	// Cmd is the object used to execute the given user CmdString to a sub-shell
	Cmd *exec.Cmd

	// TempExecFromURL is the path to a temporary file downloaded from a TaskConfig url reference
	TempExecFromURL string

	// StartTime indicates when the Cmd was started
	StartTime time.Time

	// StopTime indicates when the Cmd completed execution
	StopTime time.Time

	// EstimatedRuntime indicates the expected runtime for the given command (based off of cached values from previous runs)
	EstimatedRuntime time.Duration

	// Started indicates whether the Cmd has been attempted to run
	Started bool

	// Complete indicates whether the Cmd has been finished execution
	Complete bool

	// ReturnCode is simply the value returned from the child process after Cmd execution
	ReturnCode int

	// EnvReadFile is an extra pipe given to the child shell process for exfiltrating env vars back up to bashful (to provide as input for future tasks)
	EnvReadFile *os.File

	// Environment is a list of env vars from the exited child process
	Environment map[string]string
}

// CommandStatus represents whether a task command is about to run, already running, or has completed (in which case, was it successful or not)
type CommandStatus int32

const (
	statusRunning CommandStatus = iota
	statusPending
	statusSuccess
	statusError
)

// Color returns the ansi color value represented by the given CommandStatus
func (status CommandStatus) Color(attributes string) string {
	switch status {
	case statusRunning:
		return color.ColorCode(strconv.Itoa(config.Options.ColorRunning) + "+" + attributes)

	case statusPending:
		return color.ColorCode(strconv.Itoa(config.Options.ColorPending) + "+" + attributes)

	case statusSuccess:
		return color.ColorCode(strconv.Itoa(config.Options.ColorSuccess) + "+" + attributes)

	case statusError:
		return color.ColorCode(strconv.Itoa(config.Options.ColorError) + "+" + attributes)

	}
	return "INVALID COMMAND STATUS"
}

// CmdEvent represents an output from stdout/stderr during command execution or when a command has completed
type CmdEvent struct {
	// Task is the task which the command was run from
	Task *Task

	// Status is the current pending/running/error/success status of the command
	Status CommandStatus

	// Stdout is a single line from standard out (optional)
	Stdout string

	// Stderr is a single line from standard error (optional)
	Stderr string

	// Complete indicates if the command has exited
	Complete bool

	// ReturnCode is the sub-process return code value upon completion
	ReturnCode int
}

// LineInfo represents all template values that represent the task status
type LineInfo struct {
	// Status is the current pending/running/error/success status of the command
	Status string

	// Title is the display name to use for the task
	Title string

	// Msg may show any arbitrary string to the screen (such as stdout or stderr values)
	Msg string

	// Prefix is used to place the spinner or bullet characters before the title
	Prefix string

	// Eta is the displayed estimated time to completion based on the current time
	Eta string

	// Split can be used to "float" values to the right hand side of the screen when printing a single line
	Split string
}

// NewTask creates a new task in the context of the user configuration at a particular screen location (row)
func NewTask(taskConfig TaskConfig, displayStartIdx int, replicaValue string) *Task {
	task := Task{Config: taskConfig}
	task.inflate(displayStartIdx, replicaValue)

	for subIndex := range taskConfig.ParallelTasks {
		subTaskConfig := &taskConfig.ParallelTasks[subIndex]

		subTask := NewTask(*subTaskConfig, nextDisplayIdx, replicaValue)
		subTask.Display.Template = lineParallelTemplate
		task.Children = append(task.Children, subTask)
		nextDisplayIdx++
	}

	if len(task.Children) > 0 {
		task.Children[len(task.Children)-1].Display.Template = lineLastParallelTemplate
	}
	return &task
}

// inflate is used by the constructor to finalize task runtime values
func (task *Task) inflate(displayIdx int, replicaValue string) {

	if task.Config.CmdString != "" || task.Config.URL != "" {
		TaskStats.totalTasks++
	}

	task.inflateCmd()

	task.Display.Template = lineDefaultTemplate
	task.Display.Index = displayIdx
	task.ErrorBuffer = bytes.NewBufferString("")

	task.resultChan = make(chan CmdEvent)
	task.status = statusPending
}

func (task *Task) inflateCmd() {
	if eta, ok := config.commandTimeCache[task.Config.CmdString]; ok {
		task.Command.EstimatedRuntime = eta
	} else {
		task.Command.EstimatedRuntime = time.Duration(-1)
	}

	shell := os.Getenv("SHELL")
	if len(shell) == 0 {
		shell = "sh"
	}

	readFd, writeFd, err := os.Pipe()
	checkError(err, "Could not open env pipe for child shell")

	sudoCmd := ""
	if task.Config.Sudo {
		sudoCmd = "sudo -S "
	}
	task.Command.Cmd = exec.Command(shell, "-c", sudoCmd+task.Config.CmdString+"; BASHFUL_RC=$?; env >&3; exit $BASHFUL_RC")
	task.Command.Cmd.Stdin = strings.NewReader(string(sudoPassword) + "\n")

	// allow the child process to provide env vars via a pipe (FD3)
	task.Command.Cmd.ExtraFiles = []*os.File{writeFd}
	task.Command.EnvReadFile = readFd

	// set this command as a process group
	task.Command.Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	task.Command.ReturnCode = -1
	task.Command.Environment = map[string]string{}
}

func (task *Task) updateExec(execpath string) {
	if task.Config.CmdString == "" {
		task.Config.CmdString = config.Options.ExecReplaceString
	}
	task.Config.CmdString = strings.Replace(task.Config.CmdString, config.Options.ExecReplaceString, execpath, -1)
	task.Config.URL = strings.Replace(task.Config.URL, config.Options.ExecReplaceString, execpath, -1)

	task.inflateCmd()
}

// Kill will stop any running command (including child tasks) with a -9 signal
func (task *Task) Kill() {
	if task.Config.CmdString != "" && task.Command.Started && !task.Command.Complete {
		syscall.Kill(-task.Command.Cmd.Process.Pid, syscall.SIGKILL)
	}

	for _, subTask := range task.Children {
		if subTask.Config.CmdString != "" && subTask.Command.Started && !subTask.Command.Complete {
			syscall.Kill(-subTask.Command.Cmd.Process.Pid, syscall.SIGKILL)
		}
	}

}

// String represents the task status and command output in a single line
func (task *Task) String(terminalWidth int) string {

	if task.Command.Complete {
		task.Display.Values.Eta = ""
		if task.Command.ReturnCode != 0 && !task.Config.IgnoreFailure {
			task.Display.Values.Msg = red("Exited with error (" + strconv.Itoa(task.Command.ReturnCode) + ")")
		}
	}

	// set the name
	if task.Config.Name == "" {
		if len(task.Config.CmdString) > 25 {
			task.Config.Name = task.Config.CmdString[:22] + "..."
		} else {
			task.Config.Name = task.Config.CmdString
		}
	}

	// display
	var message bytes.Buffer

	// get a string with the summary line without a split gap or message
	task.Display.Values.Split = ""
	originalMessage := task.Display.Values.Msg
	task.Display.Values.Msg = ""
	task.Display.Template.Execute(&message, task.Display.Values)

	// calculate the max width of the message and trim it
	maxMessageWidth := terminalWidth - visualLength(message.String())
	task.Display.Values.Msg = originalMessage
	if visualLength(task.Display.Values.Msg) > maxMessageWidth {
		task.Display.Values.Msg = trimToVisualLength(task.Display.Values.Msg, maxMessageWidth-3) + "..."
	}

	// calculate a space buffer to push the eta to the right
	message.Reset()
	task.Display.Template.Execute(&message, task.Display.Values)
	splitWidth := terminalWidth - visualLength(message.String())
	if splitWidth < 0 {
		splitWidth = 0
	}

	message.Reset()

	// override the current spinner to empty or a config.Options.BulletChar
	if (!task.Command.Started || task.Command.Complete) && len(task.Children) == 0 && task.Display.Template == lineDefaultTemplate {
		task.Display.Values.Prefix = config.Options.BulletChar
	} else if task.Command.Complete {
		task.Display.Values.Prefix = ""
	}

	task.Display.Values.Split = strings.Repeat(" ", splitWidth)
	task.Display.Template.Execute(&message, task.Display.Values)

	return message.String()
}

// display prints the current task string status to the screen
func (task *Task) display() {
	terminalWidth, _ := terminal.Width()
	theScreen := newScreen()
	if config.Options.SingleLineDisplay {

		var durString, etaString, stepString, errorString string
		displayString := ""

		effectiveWidth := int(terminalWidth)

		fillColor := color.ColorCode(strconv.Itoa(config.Options.ColorSuccess) + "+i")
		emptyColor := color.ColorCode(strconv.Itoa(config.Options.ColorSuccess))
		if TaskStats.totalFailedTasks > 0 {
			fillColor = color.ColorCode(strconv.Itoa(config.Options.ColorError) + "+i")
			emptyColor = color.ColorCode(strconv.Itoa(config.Options.ColorError))
		}

		numFill := int(effectiveWidth) * TaskStats.completedTasks / TaskStats.totalTasks

		if config.Options.ShowSummaryTimes {
			duration := time.Since(startTime)
			durString = fmt.Sprintf(" Runtime[%s]", showDuration(duration))

			totalEta := time.Duration(config.totalEtaSeconds) * time.Second
			remainingEta := time.Duration(totalEta.Seconds()-duration.Seconds()) * time.Second
			etaString = fmt.Sprintf(" ETA[%s]", showDuration(remainingEta))
		}

		if TaskStats.completedTasks == TaskStats.totalTasks {
			etaString = ""
		}

		if config.Options.ShowSummarySteps {
			stepString = fmt.Sprintf(" Tasks[%d/%d]", TaskStats.completedTasks, TaskStats.totalTasks)
		}

		if config.Options.ShowSummaryErrors {
			errorString = fmt.Sprintf(" Errors[%d]", TaskStats.totalFailedTasks)
		}

		valueStr := stepString + errorString + durString + etaString

		displayString = fmt.Sprintf("%[1]*s", -effectiveWidth, fmt.Sprintf("%[1]*s", (effectiveWidth+len(valueStr))/2, valueStr))
		displayString = fillColor + displayString[:numFill] + color.Reset + emptyColor + displayString[numFill:] + color.Reset

		theScreen.Display(displayString, 0)
	} else {
		theScreen.Display(task.String(int(terminalWidth)), task.Display.Index)
	}

}

// EstimateRuntime returns the ETA in seconds until command completion
func (task *Task) EstimateRuntime() float64 {
	var etaSeconds float64
	// finalize task by appending to the set of final tasks
	if task.Config.CmdString != "" && task.Command.EstimatedRuntime != -1 {
		etaSeconds += task.Command.EstimatedRuntime.Seconds()
	}

	var maxParallelEstimatedRuntime float64
	var taskEndSecond []float64
	var currentSecond float64
	var remainingParallelTasks = config.Options.MaxParallelCmds

	for subIndex := range task.Children {
		subTask := task.Children[subIndex]
		if subTask.Config.CmdString != "" && subTask.Command.EstimatedRuntime != -1 {
			// this is a sub task with an eta
			if remainingParallelTasks == 0 {

				// we've started all possible tasks, now they should stop...
				// select the first task to stop
				remainingParallelTasks++
				minEndSecond, _, err := MinMax(taskEndSecond)
				checkError(err, "No min eta for empty array!")
				taskEndSecond = removeOneValue(taskEndSecond, minEndSecond)
				currentSecond = minEndSecond
			}

			// we are still starting tasks
			taskEndSecond = append(taskEndSecond, currentSecond+subTask.Command.EstimatedRuntime.Seconds())
			remainingParallelTasks--

			_, maxEndSecond, err := MinMax(taskEndSecond)
			checkError(err, "No max eta for empty array!")
			maxParallelEstimatedRuntime = math.Max(maxParallelEstimatedRuntime, maxEndSecond)
		}

	}
	etaSeconds += maxParallelEstimatedRuntime
	return etaSeconds
}

// CurrentEta returns a formatted string indicating a countdown until command completion
func (task *Task) CurrentEta() string {
	var eta, etaValue string

	if config.Options.ShowTaskEta {
		running := time.Since(task.Command.StartTime)
		etaValue = "Unknown!"
		if task.Command.EstimatedRuntime > 0 {
			etaValue = showDuration(time.Duration(task.Command.EstimatedRuntime.Seconds()-running.Seconds()) * time.Second)
		}
		eta = fmt.Sprintf(bold("[%s]"), etaValue)
	}
	return eta
}

// variableSplitFunc splits a bytestream based on either newline characters or by length (if the string is too long)
func variableSplitFunc(data []byte, atEOF bool) (advance int, token []byte, err error) {

	// Return nothing if at end of file and no data passed
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Case: \n
	if i := strings.Index(string(data), "\n"); i >= 0 {
		return i + 1, data[0:i], nil
	}

	// Case: \r
	if i := strings.Index(string(data), "\r"); i >= 0 {
		return i + 1, data[0:i], nil
	}

	// Case: it's just too long
	terminalWidth, _ := terminal.Width()
	if len(data) > int(terminalWidth*2) {
		return int(terminalWidth * 2), data[0:int(terminalWidth*2)], nil
	}

	// TODO: by some ansi escape sequences

	// If at end of file with data return the data
	if atEOF {
		return len(data), data, nil
	}

	return
}

// runSingleCmd executes a tasks primary command (not child task commands) and monitors command events
func (task *Task) runSingleCmd(resultChan chan CmdEvent, waiter *sync.WaitGroup, environment map[string]string) {
	logToMain("Started Task: "+task.Config.Name, infoFormat)

	task.Command.StartTime = time.Now()

	resultChan <- CmdEvent{Task: task, Status: statusRunning, ReturnCode: -1}
	waiter.Add(1)
	defer waiter.Done()

	tempFile, _ := ioutil.TempFile(config.logCachePath, "")
	task.LogFile = tempFile
	task.LogChan = make(chan LogItem)
	go singleLogger(task.LogChan, task.Config.Name, tempFile.Name())

	stdoutPipe, _ := task.Command.Cmd.StdoutPipe()
	stderrPipe, _ := task.Command.Cmd.StderrPipe()

	// copy env vars into proc
	for k, v := range environment {
		task.Command.Cmd.Env = append(task.Command.Cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	task.Command.Cmd.Start()

	readPipe := func(resultChan chan string, pipe io.ReadCloser) {
		defer close(resultChan)

		scanner := bufio.NewScanner(pipe)
		scanner.Split(variableSplitFunc)
		for scanner.Scan() {
			message := scanner.Text()
			resultChan <- vtclean.Clean(message, false)
		}
	}

	stdoutChan := make(chan string, 1000)
	stderrChan := make(chan string, 1000)
	go readPipe(stdoutChan, stdoutPipe)
	go readPipe(stderrChan, stderrPipe)

	for {
		select {
		case stdoutMsg, ok := <-stdoutChan:
			if ok {
				// it seems that we are getting a bit behind... burn off elements without showing them on the screen
				if len(stdoutChan) > 100 {
					continue
				}

				if task.Config.EventDriven {
					// this is event driven... (signal this event)
					resultChan <- CmdEvent{Task: task, Status: statusRunning, Stdout: blue(stdoutMsg), ReturnCode: -1}
				} else {
					// on a polling interval... (do not create an event)
					task.Display.Values.Msg = blue(stdoutMsg)
				}
				task.LogChan <- LogItem{Name: task.Config.Name, Message: stdoutMsg + "\n"}

			} else {
				stdoutChan = nil
			}
		case stderrMsg, ok := <-stderrChan:
			if ok {

				if task.Config.EventDriven {
					// either this is event driven... (signal this event)
					resultChan <- CmdEvent{Task: task, Status: statusRunning, Stderr: red(stderrMsg), ReturnCode: -1}
				} else {
					// or on a polling interval... (do not create an event)
					task.Display.Values.Msg = red(stderrMsg)
				}
				task.LogChan <- LogItem{Name: task.Config.Name, Message: red(stderrMsg) + "\n"}
				task.ErrorBuffer.WriteString(stderrMsg + "\n")
			} else {
				stderrChan = nil
			}
		}
		if stdoutChan == nil && stderrChan == nil {
			break
		}
	}

	returnCode := 0
	returnCodeMsg := "unknown"
	if err := task.Command.Cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// The program has exited with an exit code != 0
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				returnCode = status.ExitStatus()
			}
		} else {
			returnCode = -1
			returnCodeMsg = "Failed to run: " + err.Error()
			resultChan <- CmdEvent{Task: task, Status: statusError, Stderr: returnCodeMsg, ReturnCode: returnCode}
			task.LogChan <- LogItem{Name: task.Config.Name, Message: red(returnCodeMsg) + "\n"}
			task.ErrorBuffer.WriteString(returnCodeMsg + "\n")
		}
	}
	task.Command.StopTime = time.Now()

	logToMain("Completed Task: "+task.Config.Name+" (rc:"+strconv.Itoa(returnCode)+")", infoFormat)

	// close the write end of the pipe since the child shell is positively no longer writting to it
	task.Command.Cmd.ExtraFiles[0].Close()
	data, err := ioutil.ReadAll(task.Command.EnvReadFile)
	checkError(err, "Could not read env vars from child shell")

	if environment != nil {
		lines := strings.Split(string(data[:]), "\n")
		for _, line := range lines {
			fields := strings.SplitN(strings.TrimSpace(line), "=", 2)
			if len(fields) == 2 {
				environment[fields[0]] = fields[1]
			} else if len(fields) == 1 {
				environment[fields[0]] = ""
			}
		}
	}

	if returnCode == 0 || task.Config.IgnoreFailure {
		resultChan <- CmdEvent{Task: task, Status: statusSuccess, Complete: true, ReturnCode: returnCode}
	} else {
		resultChan <- CmdEvent{Task: task, Status: statusError, Complete: true, ReturnCode: returnCode}
		if task.Config.StopOnFailure {
			exitSignaled = true
		}
	}
}

// Pave prints the initial task (and child task) formatted status to the screen using newline characters to advance rows (not ansi control codes)
func (task *Task) Pave() {
	var message bytes.Buffer
	hasParentCmd := task.Config.CmdString != ""
	hasHeader := len(task.Children) > 0
	numTasks := len(task.Children)
	if hasParentCmd {
		numTasks++
	}
	scr := newScreen()
	scr.ResetFrame(numTasks, hasHeader, config.Options.ShowSummaryFooter)

	// make room for the title of a parallel proc group
	if hasHeader {
		message.Reset()
		lineObj := LineInfo{Status: statusRunning.Color("i"), Title: task.Config.Name, Msg: "", Prefix: config.Options.BulletChar}
		task.Display.Template.Execute(&message, lineObj)
		scr.DisplayHeader(message.String())
	}

	if hasParentCmd {
		task.Display.Values = LineInfo{Status: statusPending.Color("i"), Title: task.Config.Name}
		task.display()
	}

	for line := 0; line < len(task.Children); line++ {
		task.Children[line].Display.Values = LineInfo{Status: statusPending.Color("i"), Title: task.Children[line].Config.Name}
		task.Children[line].display()
	}
}

// StartAvailableTasks will kick start the maximum allowed number of commands (both primary and child task commands). Repeated invocation will iterate to new commands (and not repeat already completed commands)
func (task *Task) StartAvailableTasks(environment map[string]string) {
	if task.Config.CmdString != "" && !task.Command.Started && TaskStats.runningCmds < config.Options.MaxParallelCmds {
		go task.runSingleCmd(task.resultChan, &task.waiter, environment)
		task.Command.Started = true
		TaskStats.runningCmds++
	}
	for ; TaskStats.runningCmds < config.Options.MaxParallelCmds && task.lastStartedTask < len(task.Children); task.lastStartedTask++ {
		go task.Children[task.lastStartedTask].runSingleCmd(task.resultChan, &task.waiter, nil)
		task.Children[task.lastStartedTask].Command.Started = true
		TaskStats.runningCmds++
	}
}

// Completed marks a task command as being completed
func (task *Task) Completed(rc int) {
	task.Command.Complete = true
	task.Command.ReturnCode = rc
	close(task.LogChan)

	TaskStats.completedTasks++
	config.commandTimeCache[task.Config.CmdString] = task.Command.StopTime.Sub(task.Command.StartTime)
	TaskStats.runningCmds--
}

// listenAndDisplay updates the screen frame with the latest task and child task updates as they occur (either in realtime or in a polling loop). Returns when all child processes have been completed.
func (task *Task) listenAndDisplay(environment map[string]string) {
	scr := newScreen()
	// just wait for stuff to come back

	for TaskStats.runningCmds > 0 {
		select {
		case <-ticker.C:
			spinner.Next()

			if task.Config.CmdString != "" {
				if !task.Command.Complete && task.Command.Started {
					task.Display.Values.Prefix = spinner.Current()
					task.Display.Values.Eta = task.CurrentEta()
				}
				task.display()
			}

			for _, taskObj := range task.Children {
				if !taskObj.Command.Complete && taskObj.Command.Started {
					taskObj.Display.Values.Prefix = spinner.Current()
					taskObj.Display.Values.Eta = taskObj.CurrentEta()
				}
				taskObj.display()
			}

			// update the summary line
			if config.Options.ShowSummaryFooter {
				scr.DisplayFooter(footer(statusPending, ""))
			}

		case msgObj := <-task.resultChan:
			eventTask := msgObj.Task

			// update the state before displaying...
			if msgObj.Complete {
				eventTask.Completed(msgObj.ReturnCode)
				task.StartAvailableTasks(environment)
				task.status = msgObj.Status
				if msgObj.Status == statusError {
					// update the group status to indicate a failed subtask
					TaskStats.totalFailedTasks++

					// keep note of the failed task for an after task report
					task.failedTasks = append(task.failedTasks, eventTask)
				}
			}

			if !eventTask.Config.ShowTaskOutput {
				msgObj.Stderr = ""
				msgObj.Stdout = ""
			}

			if msgObj.Stderr != "" {
				eventTask.Display.Values = LineInfo{Status: msgObj.Status.Color("i"), Title: eventTask.Config.Name, Msg: msgObj.Stderr, Prefix: spinner.Current(), Eta: eventTask.CurrentEta()}
			} else {
				eventTask.Display.Values = LineInfo{Status: msgObj.Status.Color("i"), Title: eventTask.Config.Name, Msg: msgObj.Stdout, Prefix: spinner.Current(), Eta: eventTask.CurrentEta()}
			}

			eventTask.display()

			// update the summary line
			if config.Options.ShowSummaryFooter {
				scr.DisplayFooter(footer(statusPending, ""))
			} else {
				scr.MovePastFrame(false)
			}

		}

	}

	if !exitSignaled {
		task.waiter.Wait()
	}

}

// Run will run the current tasks primary command and/or all child commands. When execution has completed, the screen frame will advance.
func (task *Task) Run(environment map[string]string) {

	var message bytes.Buffer

	if !config.Options.SingleLineDisplay {
		task.Pave()
	}
	task.StartAvailableTasks(environment)
	task.listenAndDisplay(environment)

	scr := newScreen()
	hasHeader := len(task.Children) > 0 && !config.Options.SingleLineDisplay
	collapseSection := task.Config.CollapseOnCompletion && hasHeader && len(task.failedTasks) == 0

	// complete the proc group status
	if hasHeader {
		message.Reset()
		collapseSummary := ""
		if collapseSection {
			collapseSummary = purple(" (" + strconv.Itoa(len(task.Children)) + " tasks hidden)")
		}
		task.Display.Template.Execute(&message, LineInfo{Status: task.status.Color("i"), Title: task.Config.Name + collapseSummary, Prefix: config.Options.BulletChar})
		scr.DisplayHeader(message.String())
	}

	// collapse sections or parallel tasks...
	if collapseSection {

		// head to the top of the section (below the header) and erase all lines
		scr.EraseBelowHeader()

		// head back to the top of the section
		scr.MoveCursorToFirstLine()
	} else {
		// ... or this is a single task or configured not to collapse

		// instead, leave all of the text on the screen...
		// ...reset the cursor to the bottom of the section
		scr.MovePastFrame(false)
	}
}
