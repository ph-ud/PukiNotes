// Command pukinotes is a cozy, local-first whiteboard for two.
//
//	go run ./cmd/pukinotes start      launch (prints one JSON line with url, mcp, bind_code)
//	go run ./cmd/pukinotes open       what the app shortcut runs: start in the background, show the window, start the agent
//	go run ./cmd/pukinotes install    make the PukiNotes app shortcut (--agent CMD, --dock, --uninstall)
//	go run ./cmd/pukinotes status     show the running PukiNotes
//	go run ./cmd/pukinotes stop       stop it
//	go run ./cmd/pukinotes tool NAME --json '{...}'   call an agent tool (or: tool --list)
//	go run ./cmd/pukinotes join CODE  join a board from an invite code
//	go run ./cmd/pukinotes bind-code  print a fresh bind code
//	go run ./cmd/pukinotes update     pull main, rebuild, restart in place
//	go run ./cmd/pukinotes doctor     check the setup and the connection
//	go run ./cmd/pukinotes setup      rebuild the UI from web/ (optional; a built UI is included)
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const version = "0.3.0"
const defaultPort = 47321

type session struct {
	URL      string `json:"url"`
	MCP      string `json:"mcp"`
	BindCode string `json:"bind_code"`
	PID      int    `json:"pid"`
	Port     int    `json:"port"`
	Token    string `json:"token"`
	Version  string `json:"version"`
	Home     string `json:"home"`
}

func pukinotesHome() string {
	if h := os.Getenv("PUKINOTES_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(h, ".pukinotes")
}

func main() {
	log.SetFlags(0)
	cmd := "start"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "start":
		cmdStart(args)
	case "status":
		s := mustSession()
		out, err := apiGet(s, "status")
		exitOn(err)
		os.Stdout.Write(out)
	case "stop":
		s := mustSession()
		_, err := apiPost(s, "shutdown", nil)
		exitOn(err)
		fmt.Println(`{"ok":true,"stopped":true}`)
	case "update":
		s := mustSession()
		what := "update"
		if len(args) > 0 && args[0] == "--stash" {
			what = "update?stash=1" // put local code changes in git's stash first
		}
		out, err := apiPost(s, what, nil)
		exitOn(err)
		os.Stdout.Write(out)
	case "bind-code":
		s := mustSession()
		out, err := apiGet(s, "bindcode")
		exitOn(err)
		os.Stdout.Write(out)
	case "join":
		if len(args) < 1 {
			log.Fatal("usage: pukinotes join <invite-code>")
		}
		s := mustSession()
		out, err := apiPost(s, "join", map[string]string{"code": strings.Join(args, "")})
		exitOn(err)
		os.Stdout.Write(out)
	case "open":
		cmdOpen(args)
	case "install":
		cmdInstall(args)
	case "tool":
		cmdTool(args)
	case "doctor":
		cmdDoctor()
	case "setup":
		cmdSetup()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Println(helpText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, helpText)
		os.Exit(2)
	}
}

const helpText = `pukinotes: a cozy, local-first whiteboard for two. See AGENTS.md.

  start [--port N] [--no-open]   launch PukiNotes (keeps running; prints a JSON line)
  open [--no-agent]              start PukiNotes in the background if needed and show its window
  install [--agent CMD] [--dock] make the PukiNotes app shortcut (--uninstall removes it)
  status | stop | bind-code      manage the running PukiNotes
  update [--stash]               git pull main, rebuild and restart (keeps the window and agent)
  tool --list | tool NAME --json '{...}'   call an agent tool
  join CODE                      join a board from an invite code
  doctor                         check setup and connection
  setup                          rebuild the UI (optional)`

func exitOn(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":%q}`+"\n", err.Error())
		os.Exit(1)
	}
}

func readSession() (*session, error) {
	b, err := os.ReadFile(filepath.Join(pukinotesHome(), "session.json"))
	if err != nil {
		return nil, err
	}
	var s session
	return &s, json.Unmarshal(b, &s)
}

func mustSession() *session {
	s, err := readSession()
	if err == nil {
		if _, err = apiGet(s, "status"); err == nil {
			return s
		}
	}
	exitOn(errors.New("PukiNotes isn't running: start it with `go run ./cmd/pukinotes start`"))
	return nil
}

var httpc = &http.Client{Timeout: 70 * time.Second}

func apiGet(s *session, what string) ([]byte, error) {
	resp, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/api/%s/%s", s.Port, s.Token, what))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return b, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

func apiPost(s *session, what string, body any) ([]byte, error) {
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	resp, err := httpc.Post(fmt.Sprintf("http://127.0.0.1:%d/api/%s/%s", s.Port, s.Token, what), "application/json", &buf)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return b, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

func cmdTool(args []string) {
	fs := flag.NewFlagSet("tool", flag.ExitOnError)
	list := fs.Bool("list", false, "list tools and their input schemas")
	js := fs.String("json", "{}", "tool arguments as JSON")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	s := mustSession()
	if *list || name == "" {
		out, err := apiGet(s, "tools")
		exitOn(err)
		os.Stdout.Write(out)
		return
	}
	var a map[string]any
	exitOn(json.Unmarshal([]byte(*js), &a))
	out, err := apiPost(s, "tool/"+name, a)
	if err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	os.Stdout.Write(out)
}

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	port := fs.Int("port", 0, "local port (default 47321, or PUKINOTES_PORT)")
	noOpen := fs.Bool("no-open", false, "don't open the PukiNotes window")
	fs.Parse(args)
	if *port == 0 {
		*port = defaultPort
		if p, err := strconv.Atoi(os.Getenv("PUKINOTES_PORT")); err == nil {
			*port = p
		}
	}
	home := pukinotesHome()
	exitOn(os.MkdirAll(home, 0o700))
	exitOn(ensureDataFormat(home))
	afterUpdate := os.Getenv("PUKINOTES_WAIT_PORT") != ""

	// Already running? Print its session and leave it alone.
	if s, err := readSession(); err == nil && !afterUpdate {
		if st, err := apiGet(s, "status"); err == nil {
			var open struct {
				UIOpen bool `json:"ui_open"`
			}
			json.Unmarshal(st, &open)
			if b, err := apiGet(s, "bindcode"); err == nil {
				var bc struct {
					BindCode string `json:"bind_code"`
				}
				json.Unmarshal(b, &bc)
				s.BindCode = bc.BindCode
			}
			out, _ := json.Marshal(map[string]any{"url": s.URL, "mcp": s.MCP, "bind_code": s.BindCode, "pid": s.PID, "already_running": true})
			fmt.Println(string(out))
			if !*noOpen && !open.UIOpen { // a window is already showing; don't stack a second one
				openWindow(s.URL)
			}
			return
		}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	for i := 0; err != nil && afterUpdate && i < 150; i++ { // the old copy is still letting go of the port
		time.Sleep(100 * time.Millisecond)
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	}
	if err != nil {
		exitOn(fmt.Errorf("port %d is busy (another app?). Try: pukinotes start --port %d", *port, *port+1))
	}
	id, err := loadIdentity(home)
	exitOn(err)
	store, err := OpenStore(filepath.Join(home, "boards"))
	exitOn(err)
	token := randHex(16)
	if t := os.Getenv("PUKINOTES_TOKEN"); len(t) == 32 { // keep the open window working after an update
		token = t
	}
	os.Unsetenv("PUKINOTES_TOKEN")
	os.Unsetenv("PUKINOTES_WAIT_PORT")
	a := &App{home: home, port: *port, token: token, id: id, cfg: loadConfig(home), store: store,
		clients: map[*uiClient]bool{}, compactAsked: map[string]time.Time{}, stopped: make(chan struct{})}
	a.peers = newPeerManager(a)
	a.agent = newAgent(a)
	a.updater = newUpdater(a)
	go a.updater.Loop()
	reg, err := devDERPRegion()
	exitOn(err)
	a.peers.region = reg
	a.peers.Start()

	s := session{
		URL:      fmt.Sprintf("http://127.0.0.1:%d/?t=%s", *port, a.token),
		MCP:      fmt.Sprintf("http://127.0.0.1:%d/mcp/%s", *port, a.token),
		BindCode: a.agent.BindCode(), PID: os.Getpid(), Port: *port, Token: a.token, Version: version, Home: home,
	}
	sb, _ := json.MarshalIndent(s, "", "  ")
	exitOn(os.WriteFile(filepath.Join(home, "session.json"), sb, 0o600))
	line, _ := json.Marshal(map[string]any{"url": s.URL, "mcp": s.MCP, "bind_code": s.BindCode, "pid": s.PID})
	fmt.Println(string(line))

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		a.Shutdown()
	}()
	if !*noOpen {
		go func() { time.Sleep(300 * time.Millisecond); openWindow(s.URL) }()
	}
	err = a.Serve(ln)
	if s, e := readSession(); e == nil && s.PID == os.Getpid() { // after an update it belongs to the new copy
		os.Remove(filepath.Join(home, "session.json"))
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	c.Start()
}

func cmdDoctor() {
	ok := func(b bool, msg string) {
		mark := "ok  "
		if !b {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %s\n", mark, msg)
	}
	fmt.Printf("pukinotes %s, %s/%s, home %s\n", version, runtime.GOOS, runtime.GOARCH, pukinotesHome())
	s, err := readSession()
	if err != nil {
		ok(false, "PukiNotes is not running (go run ./cmd/pukinotes start)")
		return
	}
	b, err := apiGet(s, "status")
	if err != nil {
		ok(false, "PukiNotes is not responding: "+err.Error())
		return
	}
	var st struct {
		Net struct {
			Ready bool
			Error string
		}
		Agent struct {
			Bound     bool
			Name      string
			Listening bool
		}
		Peers []struct {
			Name      string
			Online    bool
			LatencyMS int `json:"latency_ms"`
		}
		UIOpen bool `json:"ui_open"`
	}
	json.Unmarshal(b, &st)
	ok(true, "PukiNotes is running at "+s.URL)
	ok(st.UIOpen, "a PukiNotes window is open (needed for agent board tools)")
	if st.Net.Ready {
		ok(true, "tailcat relay connection is up")
	} else {
		ok(false, "tailcat relay connection is down: "+st.Net.Error)
	}
	ok(st.Agent.Bound, "agent bound: "+st.Agent.Name)
	for _, p := range st.Peers {
		if p.Online {
			ok(true, fmt.Sprintf("partner %s connected (%d ms)", p.Name, p.LatencyMS))
		} else {
			ok(false, fmt.Sprintf("partner %s not connected (is their PukiNotes running?)", p.Name))
		}
	}
}

func cmdSetup() {
	if _, err := os.Stat("web/package.json"); err != nil {
		fmt.Println(`{"ok":true,"note":"run this from the repo root; the built UI is already included, so there is nothing to do"}`)
		return
	}
	if _, err := exec.LookPath("npm"); err != nil {
		fmt.Println(`{"ok":true,"note":"npm not found; using the included prebuilt UI (fine unless you changed web/src)"}`)
		return
	}
	for _, c := range [][]string{{"npm", "ci"}, {"npm", "run", "build"}} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = "web"
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		exitOn(cmd.Run())
	}
	fmt.Println(`{"ok":true,"note":"UI rebuilt into cmd/pukinotes/ui"}`)
}
