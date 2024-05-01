package meyerson

// TODO: Test *Restart actions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	webs "golang.org/x/net/websocket"
)

var (
	srvr        = &http.Server{}
	srvrRunning atomic.Bool
	conns       sync.Map

	tmplsPath, staticPath string

	errSrvrRunning    = fmt.Errorf("Server running already")
	errSrvrNotRunning = fmt.Errorf("Server not running")
)

func init() {
	_, file, _, _ := runtime.Caller(0)
	tmplsPath = filepath.Join(filepath.Dir(file), "templates")
	staticPath = filepath.Join(filepath.Dir(file), "static")
}

func newServer(addr string) *http.Server {
	return &http.Server{
		Addr: addr,
		Handler: func() http.Handler {
			r := http.NewServeMux()
			r.HandleFunc("/", homeHandler)
      r.Handle(
        "/static/",
        http.StripPrefix("/static", http.FileServer(http.Dir(staticPath))),
      )
			r.HandleFunc("/stdout/", stdoutHandler)
			r.HandleFunc("/stderr/", stderrHandler)
			r.Handle("/ws", webs.Handler(wsHandler))
			return r
		}(),
		// TODO: Discard errors?
		//ErrorLog: log.New(io.Discard, "SERVER: ", 0),
		ErrorLog: log.New(os.Stderr, "SERVER: ", 0),
	}
}

func RunWeb(addr string) error {
	if srvrRunning.Swap(true) {
		return errSrvrRunning
	}
	srvr = newServer(addr)
	go func() {
		err := srvr.ListenAndServe()
		srvrRunning.Store(false)
		if err != nil && err != http.ErrServerClosed {
			Println("Server stopped with error:", err)
		} else {
			Println("Server stopped")
		}
	}()
	return nil
}

func CloseWeb() error {
	if !srvrRunning.Load() {
		return errSrvrNotRunning
	}
	return srvr.Close()
}

func ShutdownWeb(ctx context.Context) error {
	if !srvrRunning.Load() {
		return errSrvrNotRunning
	}
	return srvr.Shutdown(ctx)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(tmplsPath, "index.html"))
}

func stdoutHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !pathpkg.IsAbs(path) {
		path = "/" + path
	}
	prefix, snum := pathpkg.Split(path)
	if prefix != "/stdout/" {
		http.NotFound(w, r)
		return
	}
	num, err := strconv.Atoi(snum)
	if err != nil {
		http.Error(w, "invalid number: "+snum, http.StatusBadRequest)
		return
	}
	proc := app.GetProcByNum(num)
	if proc == nil {
		http.Error(w, "no process with number "+snum, http.StatusNotFound)
		return
	}
	proc.procMtx.RLock()
	if proc.OutFilename == "" || proc.outFile == nil {
		w.Write([]byte(`<p style="color:red">Process stdout not captured</p>`))
		return
	}
	name := proc.outFile.Name()
	proc.procMtx.RUnlock()
	http.ServeFile(w, r, name)
}

func stderrHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !pathpkg.IsAbs(path) {
		path = "/" + path
	}
	prefix, snum := pathpkg.Split(path)
	if prefix != "/stderr/" {
		http.NotFound(w, r)
		return
	}
	num, err := strconv.Atoi(snum)
	if err != nil {
		http.Error(w, "invalid number: "+snum, http.StatusBadRequest)
		return
	}
	proc := app.GetProcByNum(num)
	if proc == nil {
		http.Error(w, "no process with number "+snum, http.StatusNotFound)
		return
	}
	proc.procMtx.RLock()
	if proc.ErrFilename == "" || proc.errFile == nil {
		w.Write([]byte(`<p style="color:red">Process stderr not captured</p>`))
		return
	}
	name := proc.errFile.Name()
	proc.procMtx.RUnlock()
	http.ServeFile(w, r, name)
}

func wsHandler(ws *webs.Conn) {
	defer ws.Close()
	conns.Store(ws.Request().RemoteAddr, ws)
	defer conns.Delete(ws.Request().RemoteAddr)
	d := json.NewDecoder(ws)
	d.UseNumber()
WsLoop:
	for srvrRunning.Load() {
		var msg Message
		if err := d.Decode(&msg); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") {
				// TODO: Print error?
			}
			return
		}
		switch msg.Action {
		case ActionAdd:
			//resp := Message{}
			errStr := ""
		ActionAddLoop:
			for _, proc := range msg.Processes {
				if proc.Name == "" {
					errStr += "missing process name" + "\n"
					continue
				} else if proc.Program == "" {
					errStr += proc.Name + ": missing program" + "\n"
					continue
				}
				for _, pair := range proc.Env {
					if pair != "" && !strings.Contains(pair, "=") {
						errStr += proc.Name + ": invalid environment variable key-value pair: " + pair + "\n"
						continue ActionAddLoop
					}
				}
				app.AddProc(proc)
				// TODO: Use startProc?
				if err := proc.Start(); err != nil {
					errStr += "error starting process: " + err.Error() + "\n"
				} else {
					//resp.Processes = append(resp.Processes, proc)
				}
			}
			if l := len(errStr); l != 0 {
				//resp.Error = errStr[:l-1]
				sendErr(ws, errStr[:l-1])
				fmt.Print(errStr)
			}
			//webs.JSON.Send(ws, msg)
		case ActionStart:
			jnum, ok := msg.Content.(json.Number)
			if !ok {
				sendErr(ws, "invalid content field, expected process num")
				continue
			}
			inum, err := jnum.Int64()
			if err != nil {
				sendErr(ws, "invalid number: "+err.Error())
				continue
			}
			num := int(inum)
			proc := app.GetProcByNum(num)
			if proc == nil {
				webs.JSON.Send(ws, Message{
					Action:  ActionDel,
					Content: num,
					Error:   "no process num: " + jnum.String(),
				})
				continue
			}
			if err := proc.Start(); err != nil {
				sendErr(ws, "error starting process: "+err.Error())
			} else {
				//webs.JSON.Send(ws, Message{Action: ActionAdd, Content: num})
			}
		case ActionDel:
			jnum, ok := msg.Content.(json.Number)
			if !ok {
				sendErr(ws, "invalid content field, expected process num")
				continue
			}
			inum, err := jnum.Int64()
			if err != nil {
				sendErr(ws, "invalid number: "+err.Error())
				continue
			}
			num := int(inum)
			if app.RemoveProcByNum(num) == nil {
				webs.JSON.Send(ws, Message{
					Action:  ActionDel,
					Content: num,
					Error:   "no process num: " + jnum.String(),
				})
			}
		case ActionInterrupt:
			interruptProcMsg(ws, msg, false)
		case ActionKill:
			killProcMsg(ws, msg, false)
		case ActionInterruptRestart:
			sendErr(ws, "not implemented")
			//interruptProcMsg(ws, msg, true)
		case ActionKillRestart:
			sendErr(ws, "not implemented")
			//killProcMsg(ws, msg, true)
		case ActionRefresh:
			if msg.Content == nil {
				if bytes, err := app.refreshProcsJSON(); err != nil {
					sendErr(ws, "internal server error: "+err.Error())
				} else {
					ws.Write(bytes)
				}
				continue
			}
			switch msg.Content.(type) {
			case []any:
				resp := Message{Action: ActionRefresh}
				var numsToDel []int
				errStr := ""
				for _, inum := range msg.Content.([]any) {
					jnum, ok := inum.(json.Number)
					if !ok {
						bytes, _ := json.Marshal(inum)
						sendErr(ws, fmt.Sprintf("invalid number: %s", bytes))
						continue WsLoop
					}
					inum, err := jnum.Int64()
					if err != nil {
						sendErr(ws, "invalid number: "+jnum.String())
						continue WsLoop
					}
					num := int(inum)
					proc := app.GetProcByNum(num)
					if proc == nil {
						errStr += "no process num: " + jnum.String() + "\n"
						numsToDel = append(numsToDel, num)
					} else {
						resp.Processes = append(resp.Processes, proc)
					}
				}
				if l := len(errStr); l != 0 {
					resp.Error = errStr[:l-1]
				}
				if len(numsToDel) != 0 {
					resp.Content = numsToDel
				}
				webs.JSON.Send(ws, resp)
			case json.Number:
				jnum := msg.Content.(json.Number)
				inum, err := jnum.Int64()
				if err != nil {
					sendErr(ws, "invalid message content")
					continue
				}
				num := int(inum)
				proc := app.GetProcByNum(num)
				if proc == nil {
					webs.JSON.Send(ws, Message{
						Action:  ActionRefresh,
						Content: []int{num},
						Error:   "no process with number " + jnum.String(),
					})
					sendErr(ws, "no process with number "+jnum.String())
				} else {
					webs.JSON.Send(ws, NewMessageProc(ActionRefresh, proc))
				}
			default:
				sendErr(ws, "invalid message content")
			}
		case ActionEnv:
			webs.JSON.Send(ws, Message{Action: ActionEnv, Content: app.env})
    case ActionConnect:
      // TODO
		default:
			sendErr(ws, fmt.Sprintf("invalid action: %s", msg.Action))
		}
	}
}

// Returns true if there was no error
func interruptProcMsg(ws *webs.Conn, msg Message, restart bool) bool {
	jnum, ok := msg.Content.(json.Number)
	if !ok {
		sendErr(ws, "invalid content field, expected process num")
		return false
	}
	inum, err := jnum.Int64()
	if err != nil {
		sendErr(ws, "invalid number: "+err.Error())
		return false
	}
	num := int(inum)
	proc := app.GetProcByNum(num)
	if proc == nil {
		webs.JSON.Send(ws, Message{
			Action:  ActionDel,
			Content: num,
			Error:   "no process num: " + jnum.String(),
		})
		return false
	}
	if err := proc.interrupt(); err != nil {
		sendErr(ws, "error interrupting process: "+err.Error())
		return false
	}
	if !restart {
		//webs.JSON.Send(ws, Message{Action: ActionInterrupt, Content: num})
		return true
	}
	if err := proc.Start(); err != nil {
		sendErr(ws, "error restarting process: "+err.Error())
		return false
	}
	//webs.JSON.Send(ws, Message{Action: ActionInterruptRestart, Content: num})
	return true
}

// Returns true if there was no error
func killProcMsg(ws *webs.Conn, msg Message, restart bool) bool {
	jnum, ok := msg.Content.(json.Number)
	if !ok {
		sendErr(ws, "invalid content field, expected process num")
		return false
	}
	inum, err := jnum.Int64()
	if err != nil {
		sendErr(ws, "invalid number: "+err.Error())
		return false
	}
	num := int(inum)
	proc := app.GetProcByNum(num)
	if proc == nil {
		webs.JSON.Send(ws, Message{
			Action:  ActionDel,
			Content: num,
			Error:   "no process num: " + jnum.String(),
		})
		return false
	}
	if err := proc.kill(); err != nil {
		sendErr(ws, "error kill process: "+err.Error())
		return false
	}
	if !restart {
		//webs.JSON.Send(ws, Message{Action: ActionKill, Content: num})
		return true
	}
	if err := proc.Start(); err != nil {
		sendErr(ws, "error restarting process: "+err.Error())
		return false
	}
	//webs.JSON.Send(ws, Message{Action: ActionKillRestart, Content: num})
	return true
}

type Message struct {
	Action    string     `json:"action"`
	Processes []*Process `json:"processes,omitempty"`
	//Content string `json:"content,omitempty"`
	Content any    `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

func NewMessageProc(action string, proc *Process) Message {
	return Message{Action: action, Processes: []*Process{proc}}
}

const (
	// FROM CLIENT:
	// Processes field should be populated
	// Server will start all processes sent.
	// FROM SERVER:
	// Processes field of message should be populated.
	ActionAdd = "add"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Content field should be populated with proc ID.
	ActionStart = "start"
	// FROM CLIENT:
	// Not sent by client
	// FROM SERVER:
	// Content field should be populated with proc ID.
	ActionFinished = "finished"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Content field should be populated with proc ID.
	ActionKill = "kill"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Content field should be populated with proc ID.
	ActionInterrupt = "interrupt"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Content field should be populated with proc ID.
	ActionDel = "del"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Not sent by server
	ActionInterruptRestart = "interrupt-restart"
	// FROM CLIENT:
	// Content field should be populated with proc ID.
	// FROM SERVER:
	// Not sent by server
	ActionKillRestart = "kill-restart"
	// FROM CLIENT:
	// Content may be populated with ID or ID array or nothing.
	// FROM SERVER:
	// Processes populated with requested procs. If a process number needs to be
	// deleted, it will be returned in an array in the content field. If [-1] is
	// sent as the content, the client should replace all their procs with what's
	// sent.
	ActionRefresh = "refresh"
	// FROM CLIENT:
	// Nothing should be populated
	// FROM SERVER:
	// Contents populated with array of strings of environment variables.
	ActionEnv = "env"
  // FROM CLIENT:
  // TODO
  // FROM SERVER:
  // Not sent by server.
  ActionConnect = "connect"
	// FROM CLIENT:
	// Not sent by client.
	// FROM SERVER:
	// Content populated with error.
	ActionError = "error"
)

func notify(msg Message) {
	conns.Range(func(_, iWs any) bool {
		webs.JSON.Send(iWs.(*webs.Conn), msg)
		return true
	})
}

func sendErr(ws *webs.Conn, msg string) {
	webs.JSON.Send(ws, Message{Action: ActionError, Error: msg})
}
