package scipipe

import (
	"strings"
)

// ExecMode specifies which execution mode should be used for a Process and
// its corresponding Tasks
type ExecMode int

const (
	// ExecModeLocal indicates that commands on the local computer
	ExecModeLocal ExecMode = iota
	// ExecModeSLURM indicates that commands should be executed on a HPC cluster
	// via a SLURM resource manager
	ExecModeSLURM ExecMode = iota
)

// Process is the central component in SciPipe after Workflow. Processes are
// long-running "services" that schedules and executes Tasks based on the IPs
// and parameters received on its in-ports and parameter ports
type Process struct {
	BaseProcess
	CommandPattern   string
	OutPortsDoStream map[string]bool
	PathFormatters   map[string]func(*Task) string
	CustomExecute    func(*Task)
	CoresPerTask     int
	ExecMode         ExecMode
	Prepend          string
	Spawn            bool
}

// ------------------------------------------------------------------------
// Factory method(s)
// ------------------------------------------------------------------------

// NewProc returns a new Process, and initializes its ports based on the
// command pattern.
func NewProc(workflow *Workflow, name string, cmd string) *Process {
	p := &Process{
		BaseProcess: NewBaseProcess(
			workflow,
			name,
		),
		CommandPattern:   cmd,
		OutPortsDoStream: make(map[string]bool),
		PathFormatters:   make(map[string]func(*Task) string),
		Spawn:            true,
		CoresPerTask:     1,
	}
	workflow.AddProc(p)
	p.initPortsFromCmdPattern(cmd, nil)
	return p
}

// initPortsFromCmdPattern is a helper function for NewProc, that sets up in-
// and out-ports based on the shell command pattern used to create the Process.
// Ports are set up in this way:
// `{i:PORTNAME}` specifies an in-port
// `{o:PORTNAME}` specifies an out-port
// `{os:PORTNAME}` specifies an out-port that streams via a FIFO file
// `{p:PORTNAME}` a "parameter (in-)port", which means a port where parameters can be "streamed"
func (p *Process) initPortsFromCmdPattern(cmd string, params map[string]string) {
	// Find in/out port names and Params and set up in struct fields
	r := getShellCommandPlaceHolderRegex()
	ms := r.FindAllStringSubmatch(cmd, -1)
	if len(ms) == 0 {
		Fail("No placeholders found in command: " + cmd)
	}
	for _, m := range ms {
		portType := m[1]
		portName := m[2]
		if portType == "o" || portType == "os" {
			p.outPorts[portName] = NewOutPort(portName)
			p.outPorts[portName].process = p
			if portType == "os" {
				p.OutPortsDoStream[portName] = true
			}
		} else if portType == "i" {
			p.inPorts[portName] = NewInPort(portName)
			p.inPorts[portName].process = p
		} else if portType == "p" {
			if params == nil || params[portName] == "" {
				p.paramInPorts[portName] = NewParamInPort(portName)
				p.paramInPorts[portName].process = p
			}
		}
	}
}

// ------------------------------------------------------------------------
// Main API methods for setting up (connecting) workflows
// ------------------------------------------------------------------------

// In is a short-form for InPort() (of BaseProcess), which works only on Process
// processes
func (p *Process) In(portName string) *InPort {
	if portName == "" && len(p.InPorts()) == 1 {
		for _, inPort := range p.InPorts() {
			return inPort // Return the (only) in-port available
		}
	}
	return p.InPort(portName)
}

// Out is a short-form for OutPort() (of BaseProcess), which works only on
// Process processes
func (p *Process) Out(portName string) *OutPort {
	if portName == "" && len(p.OutPorts()) == 1 {
		for _, outPort := range p.OutPorts() {
			return outPort // Return the (only) out-port available
		}
	}
	return p.OutPort(portName)
}

// SetPathStatic creates an (output) path formatter returning a static string file name
func (p *Process) SetPathStatic(outPortName string, path string) {
	p.PathFormatters[outPortName] = func(t *Task) string {
		path := path
		return path
	}
}

// SetPathExtend creates an (output) path formatter that extends the path of
// an input IP
func (p *Process) SetPathExtend(inPortName string, outPortName string,
	extension string) {
	p.PathFormatters[outPortName] = func(t *Task) string {
		extension := extension
		return t.InPath(inPortName) + extension
	}
}

// SetPathReplace creates an (output) path formatter that uses an input's path
// but replaces parts of it.
func (p *Process) SetPathReplace(inPortName string, outPortName string, old string, new string) {
	p.PathFormatters[outPortName] = func(t *Task) string {
		old := old
		new := new
		return strings.Replace(t.InPath(inPortName), old, new, -1)
	}
}

// SetPathCustom takes a function which produces a file path based on data
// available in *Task, such as concrete file paths and parameter values,
func (p *Process) SetPathCustom(outPortName string, pathFmtFunc func(task *Task) (path string)) {
	p.PathFormatters[outPortName] = pathFmtFunc
}

// ------------------------------------------------------------------------
// Run method
// ------------------------------------------------------------------------

// Run runs the process by instantiating and executing Tasks for all inputs
// and parameter values on its in-ports. in the case when there are no inputs
// or parameter values on the in-ports, it will run just once before it
// terminates. note that the actual execution of shell commands are done inside
// Task.Execute, not here.
func (p *Process) Run() {
	// Check that CoresPerTask is a sane number
	if p.CoresPerTask > cap(p.workflow.concurrentTasks) {
		Failf("%s: CoresPerTask (%d) can't be greater than maxConcurrentTasks of workflow (%d)\n", p.Name(), p.CoresPerTask, cap(p.workflow.concurrentTasks))
	}

	defer p.CloseOutPorts()

	tasks := []*Task{}
	Debug.Printf("Process %s: Starting to create and schedule tasks\n", p.name)
	for t := range p.createTasks() {

		// Collect created tasks, for the second round
		// where tasks are waited for to finish, before
		// sending their outputs.
		Debug.Printf("Process %s: Instantiated task [%s] ...", p.name, t.Command)
		tasks = append(tasks, t)

		anyPreviousFifosExists := t.anyFifosExist()

		if p.ExecMode == ExecModeLocal {
			if !anyPreviousFifosExists {
				Debug.Printf("Process %s: No FIFOs existed, so creating, for task [%s] ...", p.name, t.Command)
				t.createFifos()
			}

			// Sending FIFOs for the task
			for oname, oip := range t.OutIPs {
				if oip.doStream {
					p.Out(oname).Send(oip)
				}
			}
		}

		if anyPreviousFifosExists {
			Debug.Printf("Process %s: Previous FIFOs existed, so not executing task [%s] ...\n", p.name, t.Command)
			// Since t.Execute() is not run, that normally sends the Done signal, we
			// have to send it manually here:
			go func() {
				defer close(t.Done)
				t.Done <- 1
			}()
		} else {
			Debug.Printf("Process %s: Go-Executing task in separate go-routine: [%s] ...\n", p.name, t.Command)
			// Run the task
			go t.Execute()
			Debug.Printf("Process %s: Done go-executing task in go-routine: [%s] ...\n", p.name, t.Command)
		}
	}

	Debug.Printf("Process %s: Starting to loop over %d tasks to send out IPs ...\n", p.name, len(tasks))
	for _, t := range tasks {
		Debug.Printf("Process %s: Waiting for Done from task: [%s]\n", p.name, t.Command)
		<-t.Done
		Debug.Printf("Process %s: Received Done from task: [%s]\n", p.name, t.Command)
		for oname, oip := range t.OutIPs {
			if !oip.doStream {
				Debug.Printf("Process %s: Sending IPs on outport %s, for task [%s] ...\n", p.name, oname, t.Command)
				p.Out(oname).Send(oip)
				Debug.Printf("Process %s: Done sending IPs on outport %s, for task [%s] ...\n", p.name, oname, t.Command)
			}
		}
	}
}

// createTasks is a helper method for the Run method that creates tasks based on
// in-coming IPs on the in-ports, and feeds them to the Run method on the
// returned channel ch
func (p *Process) createTasks() (ch chan *Task) {
	ch = make(chan *Task)
	go func() {
		defer close(ch)
		for {
			inIPs, inPortsOpen := p.receiveOnInPorts()
			Debug.Printf("Process.createTasks:%s Got inIPs: %v", p.name, inIPs)
			params, paramPortsOpen := p.receiveOnParamInPorts()
			Debug.Printf("Process.createTasks:%s Got params: %s", p.name, params)
			if !inPortsOpen && !paramPortsOpen {
				Debug.Printf("Process.createTasks:%s Breaking: Both inPorts and paramInPorts closed", p.name)
				break
			}
			if len(p.inPorts) == 0 && !paramPortsOpen {
				Debug.Printf("Process.createTasks:%s Breaking: No inports, and params closed", p.name)
				break
			}
			if len(p.paramInPorts) == 0 && !inPortsOpen {
				Debug.Printf("Process.createTasks:%s Breaking: No params, and inPorts closed", p.name)
				break
			}
			t := NewTask(p.workflow, p, p.Name(), p.CommandPattern, inIPs, p.PathFormatters, p.OutPortsDoStream, params, p.Prepend, p.ExecMode, p.CoresPerTask)
			if p.CustomExecute != nil {
				t.CustomExecute = p.CustomExecute
			}
			ch <- t
			if len(p.inPorts) == 0 && len(p.paramInPorts) == 0 {
				Debug.Printf("Process.createTasks:%s Breaking: No inports nor params", p.name)
				break
			}
		}
		Debug.Printf("Process.createTasks:%s Did break", p.name)
	}()
	return ch
}
