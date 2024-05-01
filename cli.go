package meyerson

// TODO: Make it so output files can't be overwritten?
// TODO: Make it so the output file names can incorporate the special values,
// e.g., -.log for stderr is process1-stderr.log

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	app            = NewApp()
	errProcRunning = fmt.Errorf("process running already")
)

func Run(args []string) {
	log.SetFlags(0)

	fs := flag.NewFlagSet("meyerson", flag.ExitOnError)
	outDir := fs.String("out-dir", "", "Directory to put output files in")
	configPath := fs.String("config", "", "Path to config file")
	shortConfigPath := fs.String("c", "", "Same as --config")
	configTemp := fs.Bool(
		"config-template", false,
		"Generate a template config file in the current directory",
	)
	shortConfigTemp := fs.Bool(
		"t", false,
		"Generate a template config file in the current directory (same as config-template)",
	)
	bareConfigTemp := fs.Bool(
		"bare-config-template", false,
		"Generate a template config file without comments in the current directory",
	)
	shortBareConfigTemp := fs.Bool(
		"b", false,
		"Generate a template config file without comments in the current directory (same as bare-config-template)",
	)
	configTomlTemp := fs.Bool(
		"config-toml-template", false,
		"Generate a template config file in the current directory",
	)
	shortConfigTomlTemp := fs.Bool(
		"T", false,
		"Generate a template config file in the current directory (same as config-toml-template)",
	)
	bareConfigTomlTemp := fs.Bool(
		"bare-config-toml-template", false,
		"Generate a template config file without comments in the current directory",
	)
	shortBareConfigTomlTemp := fs.Bool(
		"B", false,
		"Generate a template config file without comments in the current directory (same as bare-config-toml-template)",
	)
	addr := fs.String(
		"addr", "",
		"Address to run server on, if passed, overriding config file serverAddr",
	)
	fs.Parse(args)

	if *configPath == "" {
		*configPath = *shortConfigPath
	}

	intChan := make(chan os.Signal, 1)
	go func() {
		<-intChan
		app.procsMtx.RLock()
		for _, proc := range app.procs {
			if err := proc.interrupt(); err != nil {
				log.Printf(
					"error interrupting process %d (%s): %v",
					proc.Num, proc.Name, err,
				)
			}
		}
		app.procsMtx.RUnlock()
		done := app.Done()
		select {
		case _, _ = <-done:
			os.Exit(0)
		case <-intChan:
		}
		app.procsMtx.RLock()
		for _, proc := range app.procs {
			proc.kill()
		}
		app.procsMtx.RUnlock()
		os.Exit(0)
	}()
	signal.Notify(intChan, os.Interrupt)
	termChan := make(chan os.Signal, 1)
	go func() {
		<-termChan
		app.procsMtx.RLock()
		for _, proc := range app.procs {
			// TODO: Send sigterm?
			proc.kill()
		}
		app.procsMtx.RLock()
		os.Exit(0)
	}()
	signal.Notify(termChan, syscall.SIGTERM)

	if *configTemp || *shortConfigTemp {
		_, thisFile, _, _ := runtime.Caller(0)
		configPath := filepath.Join(
			filepath.Dir(thisFile), "config-templates", "meyerson-comments.json",
		)
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Fatal("error reading config template: ", err)
		}
		if err = os.WriteFile("meyerson.json", data, 0666); err != nil {
			log.Fatal("error writing config template: ", err)
		}
		return
	} else if *bareConfigTemp || *shortBareConfigTemp {
		_, thisFile, _, _ := runtime.Caller(0)
		configPath := filepath.Join(
      filepath.Dir(thisFile), "config-templates", "meyerson.json",
    )
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Fatal("error reading config template: ", err)
		}
		if err = os.WriteFile("meyerson.json", data, 0666); err != nil {
			log.Fatal("error writing config template: ", err)
		}
		return
	} else if *configTomlTemp || *shortConfigTomlTemp {
		_, thisFile, _, _ := runtime.Caller(0)
		configPath := filepath.Join(
			filepath.Dir(thisFile), "config-templates", "meyerson-comments.toml",
		)
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Fatal("error reading config template: ", err)
		}
		if err = os.WriteFile("meyerson.toml", data, 0666); err != nil {
			log.Fatal("error writing config template: ", err)
		}
		return
	} else if *bareConfigTomlTemp || *shortBareConfigTomlTemp {
		_, thisFile, _, _ := runtime.Caller(0)
		configPath := filepath.Join(
      filepath.Dir(thisFile), "config-templates", "meyerson.toml",
    )
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Fatal("error reading config template: ", err)
		}
		if err = os.WriteFile("meyerson.toml", data, 0666); err != nil {
			log.Fatal("error writing config template: ", err)
		}
		return
	}

	if *configPath != "" {
		ext := filepath.Ext(*configPath)
		config := &Config{}
		switch ext {
		case ".json":
			f, err := os.Open(*configPath)
			if err != nil {
				log.Fatal("error opening config file: ", err)
			}
			err = json.NewDecoder(f).Decode(config)
			f.Close()
			if err != nil {
				log.Fatal("error parsing config file: ", err)
			}
		case ".toml":
			if _, err := toml.DecodeFile(*configPath, config); err != nil {
				log.Fatal("error parsing config file: ", err)
			}
		default:
			log.Fatal("invalid config file, expected .json or .toml file")
		}
		if *addr != "" {
			config.ServerAddr = *addr
		}
		app = AppFromConfig(config)
		if *outDir != "" {
			app.outDir = *outDir
		}
		if len(app.procs) != 0 {
			Println("Starting processes...")
			app.StartProcs()
		}
		handleInput()
		app.Wait()
		return
	}

	app.outDir = *outDir

	// Create the processes
	for i := 1; true; {
		proc, stop := getProcessFromStdin(i)
		if stop {
			break
		} else if proc == nil {
			continue
		}
		app.AddProc(proc)

		if confirm("Start now [Y/n]? ") {
			Printf("Starting process %d (%s)\n", proc.Num, proc.Name)
			if err := proc.Start(); err != nil {
				Printf(
					"Error starting process %d (%s): %v\n", proc.Num, proc.Name, err,
				)
			}
		}

		Println("====================")
		i++
	}
	Println("========================================")

	// Check for any deletions
	for {
		if name := readline("Delete any procs (enter name)? "); name == "" {
			break
		} else if !app.RemoveProc(name) {
			Println("No process with name: ", name)
		}
	}
	Println("========================================")

	// Start the processes and server, if necessary
	Println("Starting (remaining) processes...")
	app.StartProcs()
	if *addr != "" {
		fmt.Println("Starting server on", *addr)
		RunWeb(*addr)
	}
	handleInput()
	app.Wait()
}

// False means the loop when initially creating the processes should break
func getProcessFromStdin(num int) (*Process, bool) {
	proc := &Process{app: app, Num: num}
	if num != -1 {
		proc.Name = readline(fmt.Sprintf("Process %d Name: ", num))
	} else {
		proc.Name = readline("Process Name: ")
	}
	if proc.Name == "" {
		return nil, false
	}
	proc.Program = readline("Program: ")

	for i := 1; true; i++ {
		if arg := readline(fmt.Sprintf("Arg %d: ", i)); arg != "" {
			proc.Args = append(proc.Args, arg)
		} else {
			break
		}
	}

	proc.Env = os.Environ()
	for {
		if kv := readline(fmt.Sprintf("Env Var (key=val): ")); kv != "" {
			proc.Env = append(proc.Env, kv)
		} else {
			break
		}
	}

	proc.OutFilename = readline(
		"Stdout output filename (- = process number, % = name): ",
	)
	proc.ErrFilename = readline(
		"Stderr output filename (- = process number, % = name): ",
	)

	if !confirm("Ok [Y/n]? ") {
		return nil, true
	}
	return proc, true
}

func handleInput() {
	Println("Pause output (p) to enter commands | Ctrl-C to quit")

	// Wait for input
	printChoices := func() {
		fmt.Println("Options")
		fmt.Println("1) Print Options (Print This)")
		fmt.Println("2) Print Processes")
		fmt.Println("3) Restart Process (Kill)")
		fmt.Println("4) Restart Process (Interrupt)")
		fmt.Println("5) Kill Process")
		fmt.Println("6) Interrupt Process")
		fmt.Println("7) Add Process")
		fmt.Println("8) Add Process")
		fmt.Println("9) Start Server")
		fmt.Println("10) Close Server")
		fmt.Println("11) Shutdown Server")
		fmt.Println("12) Server Address")
		fmt.Println("0) Resume Output")
		fmt.Println("-1) Wait for procs and quit")
	}
InputLoop:
	for {
		line := strings.ToLower(readline())
		if line != "p" && line != "P" {
			continue
		}
		stdout.Lock()
		printChoices()
		for {
			for {
				choice, err := strconv.Atoi(readline("Choice: "))
				if err != nil {
					fmt.Println("Invalid choice")
					continue
				}
				switch choice {
				case 1:
					printChoices()
				case 2:
					printProcesses()
				case 3:
					restartProcessKill()
				case 4:
					restartProcessInterrupt()
				case 5:
					killProcess()
				case 6:
					interruptProcess()
				case 7:
					addProcess()
				case 8:
					delProcess()
				case 9:
					addr := readline("Address: ")
					if addr == "" {
						continue
					}
					if err := RunWeb(addr); err != nil {
						fmt.Println(err)
					} else {
						fmt.Println("Starting server on", addr)
					}
				case 10:
					if !confirm("Close server immediately [Y/n]?") {
						continue
					}
					if err := CloseWeb(); err != nil {
						fmt.Println(err)
					}
				case 11:
					// TODO: Context
					if !confirm("Shutdown server gracefully [Y/n]?") {
						continue
					}
					if err := ShutdownWeb(context.Background()); err != nil {
						fmt.Println(err)
					}
				case 12:
					if srvrRunning.Load() {
						fmt.Printf("%s (RUNNING)\n", srvr.Addr)
					} else {
						fmt.Printf("%s (NOT RUNNING)\n", srvr.Addr)
					}
				case 0:
					stdout.Unlock()
					continue InputLoop
				case -1:
					stdout.Unlock()
					break InputLoop
				default:
					fmt.Println("Invalid choice")
					continue
				}
				break
			}
		}
		stdout.Unlock()
	}
}

func printProcesses() {
	for _, proc := range app.procs {
		fmt.Printf(
			"Process #%d (%s): %s\n",
			proc.Num, proc.Name, statusString(proc.status.Load()),
		)
	}
}

func restartProcessKill() {
	for {
		num, err := strconv.Atoi(readline("Process # (-1 = Back): "))
		if err != nil {
			fmt.Println("Invalid number")
		}
		if num == -1 {
			return
		}
		proc := app.GetProcByNum(num)
		if proc == nil {
			fmt.Println("No process with num", num)
			continue
		}
		if err := proc.kill(); err != nil {
			fmt.Println("Error killing process:", err)
			continue
		}
		if err := startProc(proc); err != nil {
			fmt.Println("Error starting process:", err)
		}
	}
}

func restartProcessInterrupt() {
	for {
		num, err := strconv.Atoi(readline("Process # (-1 = Back): "))
		if err != nil {
			fmt.Println("Invalid number")
		}
		if num == -1 {
			return
		}
		proc := app.GetProcByNum(num)
		if proc == nil {
			fmt.Println("No process with num", num)
			continue
		}
		if err := proc.interrupt(); err != nil {
			fmt.Println("Error interrupt process:", err)
			continue
		}
		if err := startProc(proc); err != nil {
			fmt.Println("Error starting process:", err)
		}
	}
}

func killProcess() {
	for {
		num, err := strconv.Atoi(readline("Process # (-1 = Back): "))
		if err != nil {
			fmt.Println("Invalid number")
		}
		if num == -1 {
			return
		}
		proc := app.GetProcByNum(num)
		if proc == nil {
			fmt.Println("No process with num", num)
			continue
		}
		if err := proc.interrupt(); err != nil {
			fmt.Println("Error killing process:", err)
		}
	}
}

func interruptProcess() {
	for {
		num, err := strconv.Atoi(readline("Process # (-1 = Back): "))
		if err != nil {
			fmt.Println("Invalid number")
		}
		if num == -1 {
			return
		}
		proc := app.GetProcByNum(num)
		if proc == nil {
			fmt.Println("No process with num", num)
			continue
		}
		if err := proc.interrupt(); err != nil {
			fmt.Println("Error interrupt process:", err)
		}
	}
}

func addProcess() {
	// TODO: Option to start all at once?
	for {
		proc, stop := getProcessFromStdin(-1)
		if stop {
			break
		}
		proc.Num = app.getNextNum()
		if proc != nil {
			app.AddProc(proc)
			fmt.Print("Added process ", proc.Num)
			if err := startProc(proc); err != nil {
				fmt.Println("Error starting process:", err)
			}
		}
	}
}

func delProcess() {
	for {
		num, err := strconv.Atoi(readline("Process # (-1 = Back): "))
		if err != nil {
			fmt.Println("Invalid number")
		}
		if num == -1 {
			return
		}
		proc := app.RemoveProcByNum(num)
		if proc != nil {
			fmt.Println("No process with num", num)
			continue
		}
		// TODO: allow interrupt
		if err := proc.kill(); err != nil {
			fmt.Println("Error killing process:", err)
		}
	}
}

func startProc(proc *Process) error {
	// TODO: Do we need to wait?
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if err := proc.Start(); err != errProcRunning {
			return err
		}
	}
	return fmt.Errorf("failed to start too many times")
}

type Config struct {
	ServerAddr string     `json:"serverAddr,omitempty" toml:"server-addr"`
	OutDir     string     `json:"outDir,omitempty" toml:"out-dir"`
	Env        []string   `json:"env,omitempty" toml:"env"`
	Procs      []*Process `json:"procs,omitempty" toml:"proc"`
}

type App struct {
	procs       []*Process
	env         []string
	nextProcNum int
	procsMtx    sync.RWMutex
	outDir      string
	waitOnce    sync.Once
	wg          sync.WaitGroup
}

func NewApp() *App {
	return &App{env: os.Environ(), nextProcNum: 1}
}

func AppFromConfig(config *Config) *App {
	app := NewApp()
	app.env = append(app.env, config.Env...)
	app.outDir = config.OutDir
	app.nextProcNum = 1
	for _, proc := range config.Procs {
		app.AddProc(proc)
	}
	if config.ServerAddr != "" {
		fmt.Println("Starting server on ", config.ServerAddr)
		RunWeb(config.ServerAddr)
	}
	return app
}

func (a *App) AddProc(p *Process) {
	env := make([]string, len(a.env), len(a.env)+len(p.Env))
	copy(env, a.env)
	p.Env = append(env, p.Env...)
	p.app = a
	a.procsMtx.Lock()
	p.Num = a.nextProcNum
	a.nextProcNum++
	a.procs = append(a.procs, p)
	a.procsMtx.Unlock()
	notify(NewMessageProc(ActionAdd, p))
}

func (a *App) GetProcByNum(num int) *Process {
	a.procsMtx.RLock()
	defer a.procsMtx.RUnlock()
	for _, proc := range a.procs {
		if proc.Num == num {
			return proc
		}
	}
	return nil
}

// Returns true if a process was deleted
func (a *App) RemoveProc(name string) bool {
	a.procsMtx.Lock()
	defer a.procsMtx.Unlock()
	for i, proc := range a.procs {
		if proc.Name == name {
			a.procs = append(a.procs[:i], a.procs[i+1:]...)
			notify(Message{Action: ActionDel, Content: proc.Num})
			return true
		}
	}
	return false
}

// Returns non-nill if a process was deleted
func (a *App) RemoveProcByNum(num int) *Process {
	a.procsMtx.Lock()
	defer a.procsMtx.Unlock()
	for i, proc := range a.procs {
		if proc.Num == num {
			a.procs = append(a.procs[:i], a.procs[i+1:]...)
			notify(Message{Action: ActionDel, Content: proc.Num})
			return proc
		}
	}
	return nil
}

func (a *App) StartProcs() {
	a.procsMtx.RLock()
	defer a.procsMtx.RUnlock()
	for _, proc := range a.procs {
		if proc.Delay != 0 {
			time.Sleep(time.Second * proc.Delay)
		}
		if err := proc.Start(); err != nil && err != errProcRunning {
			Printf(
				"error starting process %d (%s): %v\n", proc.Num, proc.Name, err,
			)
		}
	}
}

// Done returns as channel that is closed whenever all processes are done
// (app.Wait() returns)
func (a *App) Done() <-chan struct{} {
	ch := make(chan struct{})
	a.waitOnce.Do(func() {
		a.Wait()
		close(ch)
	})
	return ch
}

func (a *App) Wait() {
	a.wg.Wait()
}

func (a *App) getNextNum() int {
	a.procsMtx.Lock()
	n := a.nextProcNum
	a.nextProcNum++
	a.procsMtx.Unlock()
	return n
}

func (a *App) refreshProcsJSON() ([]byte, error) {
	a.procsMtx.RLock()
	defer a.procsMtx.RUnlock()
	return json.Marshal(Message{
		Action:    ActionRefresh,
		Processes: a.procs,
		Content:   []int{-1},
	})
}

type Process struct {
	Name        string        `json:"name" toml:"name"`
	Program     string        `json:"program" toml:"program"`
	Args        []string      `json:"args,omitempty" toml:"args"`
	Env         []string      `json:"env,omitempty" toml:"env"`
	OutFilename string        `json:"outFilename,omitempty" toml:"out-filename"`
	ErrFilename string        `json:"errFilename,omitempty" toml:"err-filename"`
	Delay       time.Duration `json:"delay,omitempty" toml:"delay"`
	// TODO: use
	Dir string `json:"dir,omitempty" toml:"dir"`
	Num int    `json:"num" toml:"-"`

	app              *App
	cmd              *exec.Cmd
	cancelFunc       context.CancelFunc
	outFile, errFile *os.File
	// Mutex for all from app to here
	procMtx sync.RWMutex
	status  atomic.Uint32
}

func (p *Process) MarshalJSON() ([]byte, error) {
	name, _ := json.Marshal(p.Name)
	prog, _ := json.Marshal(p.Program)
	args, _ := json.Marshal(p.Args)
	env, _ := json.Marshal(p.Env)
	dir, _ := json.Marshal(p.Dir)
	outFilename, _ := json.Marshal(p.OutFilename)
	errFilename, _ := json.Marshal(p.ErrFilename)
	num, _ := json.Marshal(p.Num)
	status, _ := json.Marshal(statusString(p.status.Load()))
	return []byte(fmt.Sprintf(
		`{"name":%s,"program":%s,"args":%s,"env":%s,"dir":%s,"outFilename":%s,"errFilename":%s,"num":%s,"status":%s}`,
		name, prog, args, env, dir, outFilename, errFilename, num, status,
	)), nil
}

func (p *Process) populateCmd() {
	if p.status.Load() == statusRunning {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cmd = exec.CommandContext(ctx, p.Program, p.Args...)
	p.cancelFunc, p.cmd.Env = cancel, p.Env
}

func (p *Process) kill() error {
	if !p.status.CompareAndSwap(statusRunning, statusFinished) {
		return nil
	}
	if p.cmd != nil && p.cmd.Process != nil {
		err := p.cmd.Process.Signal(os.Kill)
		notify(Message{Action: ActionKill, Content: p.Num})
		return err
	}
	return nil
}

func (p *Process) interrupt() error {
	if !p.status.CompareAndSwap(statusRunning, statusFinished) {
		return nil
	}
	if p.cmd != nil && p.cmd.Process != nil {
		err := p.cmd.Process.Signal(os.Interrupt)
		notify(Message{Action: ActionInterrupt, Content: p.Num})
		return err
	}
	return nil
}

func (p *Process) Start() error {
	if p.status.Load() == statusRunning {
		return errProcRunning
	}
	p.procMtx.Lock()
	defer p.procMtx.Unlock()
	p.populateCmd()
	var err error
	// Open the files for output
	if p.OutFilename != "" {
		if p.OutFilename == "-" {
			p.OutFilename = fmt.Sprintf("process%d-stdout.txt", p.Num)
		} else if p.OutFilename == "%" {
			p.OutFilename = fmt.Sprintf("%s-stdout.txt", p.Name)
		}
		p.outFile, err = os.Create(filepath.Join(p.app.outDir, p.OutFilename))
		if err != nil {
			Printf(
				"Error creating stdout output file for %s: %v\n",
				p.Name, err,
			)
		}
		p.cmd.Stdout = p.outFile
	}
	if p.ErrFilename != "" {
		if p.ErrFilename == "-" {
			p.ErrFilename = fmt.Sprintf("process%d-stderr.txt", p.Num)
		} else if p.ErrFilename == "%" {
			p.ErrFilename = fmt.Sprintf("%s-stderr.txt", p.Name)
		} else if p.ErrFilename == p.OutFilename {
			// Same file
			p.ErrFilename = p.OutFilename
			goto StartProc
		}
		p.errFile, err = os.Create(filepath.Join(p.app.outDir, p.ErrFilename))
		if err != nil {
			Printf(
				"Error creating stderr output file for %s: %v\n",
				p.Name, err,
			)
		}
		p.cmd.Stderr = p.errFile
	}
StartProc:
	// Start the process
	if err := p.cmd.Start(); err != nil {
		p.status.Store(statusFinished)
		// Delete the created files
		if p.outFile != nil {
			p.outFile.Close()
			if err := os.Remove(p.outFile.Name()); err != nil {
				Printf("Error removing stdout file for %s: %v\n", p.Name, err)
			}
		}
		if p.errFile != nil {
			p.errFile.Close()
			if err := os.Remove(p.errFile.Name()); err != nil {
				Printf("Error removing stderr file for %s: %v\n", p.Name, err)
			}
		}
		return err
	}
	p.status.Store(statusRunning)
	p.app.wg.Add(1)
	// Wait for the process to finish
	go func() {
		notify(NewMessageProc(ActionStart, p))
		p.Wait()
	}()
	return nil
}

func (p *Process) Wait() {
	err := p.cmd.Wait()
	alreadyDone := p.status.Swap(statusFinished) != statusRunning
	notify(Message{Action: ActionFinished, Content: p.Num})
	// TODO: Handle error better to ignore when the user stops the program
	if err != nil && !alreadyDone {
		Printf("%s terminated with error: %v\n", p.Name, err)
	} else {
		Println(p.Name, "finished")
	}
	// Close the files
	if p.outFile != nil {
		p.outFile.Close()
	}
	if p.errFile != nil {
		p.errFile.Close()
	}
	p.app.wg.Done()
}

const (
	statusNotStarted uint32 = iota
	statusRunning
	statusFinished
)

func statusString(u uint32) string {
	switch u {
	case statusNotStarted:
		return "NOT STARTED"
	case statusRunning:
		return "RUNNING"
	case statusFinished:
		return "FINISHED"
	default:
		return "UNKNOWN"
	}
}

var stdinReader = bufio.NewReader(os.Stdin)

func readline(prompt ...string) string {
	if len(prompt) != 0 {
		fmt.Print(prompt[0])
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(line)
}

func confirm(prompt string) bool {
	conf := strings.ToLower(readline(prompt))
	return conf == "y" || conf == "yes"
}

type LockedWriter struct {
	w io.Writer
	sync.Mutex
}

func NewLockedWriter(w io.Writer) *LockedWriter {
	return &LockedWriter{w: w}
}

func (s *LockedWriter) Write(p []byte) (int, error) {
	s.Lock()
	defer s.Unlock()
	return s.LockedWrite(p)
}

func (s *LockedWriter) LockedWrite(p []byte) (int, error) {
	return s.w.Write(p)
}

var (
	stdout = NewLockedWriter(os.Stdout)
	stderr = NewLockedWriter(os.Stderr)
)

func Print(args ...any) (int, error) {
	return fmt.Fprint(stdout, args...)
}

func Printf(format string, args ...any) (int, error) {
	return fmt.Fprintf(stdout, format, args...)
}

func Println(args ...any) (int, error) {
	return fmt.Fprintln(stdout, args...)
}
