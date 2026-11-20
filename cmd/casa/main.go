// Command casa is a cozy, local-first whiteboard for two.
//
//	go run ./cmd/casa start      launch (prints one JSON line with url, mcp, bind_code)
//	go run ./cmd/casa status     show the running Casa
//	go run ./cmd/casa stop       stop it
//	go run ./cmd/casa tool NAME --json '{...}'   call an agent tool (or: tool --list)
//	go run ./cmd/casa join CODE  join a board from an invite code
//	go run ./cmd/casa bind-code  print a fresh bind code
//	go run ./cmd/casa doctor     check the setup and the connection
//	go run ./cmd/casa setup      rebuild the UI from web/ (optional; a built UI is included)
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

const version = "0.1.0"
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

func casaHome() string {
	if h := os.Getenv("CASA_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(h, ".casa")
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
	case "bind-code":
		s := mustSession()
		out, err := apiGet(s, "bindcode")
		exitOn(err)
		os.Stdout.Write(out)
	case "join":
		if len(args) < 1 {
			log.Fatal("usage: casa join <invite-code>")
		}
		s := mustSession()
		out, err := apiPost(s, "join", map[string]string{"code": strings.Join(args, "")})
		exitOn(err)
		os.Stdout.Write(out)
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

const helpText = `casa: a cozy, local-first whiteboard for two. See AGENTS.md.

  start [--port N] [--no-open]   launch Casa (keeps running; prints a JSON line)
  status | stop | bind-code      manage the running Casa
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
	b, err := os.ReadFile(filepath.Join(casaHome(), "session.json"))
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
	exitOn(errors.New("Casa isn't running: start it with `go run ./cmd/casa start`"))
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
	port := fs.Int("port", 0, "local port (default 47321, or CASA_PORT)")
	noOpen := fs.Bool("no-open", false, "don't open a browser window")
	fs.Parse(args)
	if *port == 0 {
		*port = defaultPort
		if p, err := strconv.Atoi(os.Getenv("CASA_PORT")); err == nil {
			*port = p
		}
	}
	home := casaHome()
	exitOn(os.MkdirAll(home, 0o700))

	// Already running? Print its session and leave it alone.
	if s, err := readSession(); err == nil {
		if _, err := apiGet(s, "status"); err == nil {
			if b, err := apiGet(s, "bindcode"); err == nil {
				var bc struct {
					BindCode string `json:"bind_code"`
				}
				json.Unmarshal(b, &bc)
				s.BindCode = bc.BindCode
			}
			out, _ := json.Marshal(map[string]any{"url": s.URL, "mcp": s.MCP, "bind_code": s.BindCode, "pid": s.PID, "already_running": true})
			fmt.Println(string(out))
			if !*noOpen {
				openBrowser(s.URL)
			}
			return
		}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		exitOn(fmt.Errorf("port %d is busy (another app?). Try: casa start --port %d", *port, *port+1))
	}
	id, err := loadIdentity(home)
	exitOn(err)
	store, err := OpenStore(filepath.Join(home, "boards"))
	exitOn(err)
	a := &App{home: home, port: *port, token: randHex(16), id: id, cfg: loadConfig(home), store: store,
		clients: map[*uiClient]bool{}, compactAsked: map[string]time.Time{}, stopped: make(chan struct{})}
	a.peers = newPeerManager(a)
	a.agent = newAgent(a)
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
		go func() { time.Sleep(300 * time.Millisecond); openBrowser(s.URL) }()
	}
	err = a.Serve(ln)
	os.Remove(filepath.Join(home, "session.json"))
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
	fmt.Printf("casa %s, %s/%s, home %s\n", version, runtime.GOOS, runtime.GOARCH, casaHome())
	s, err := readSession()
	if err != nil {
		ok(false, "Casa is not running (go run ./cmd/casa start)")
		return
	}
	b, err := apiGet(s, "status")
	if err != nil {
		ok(false, "Casa is not responding: "+err.Error())
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
	ok(true, "Casa is running at "+s.URL)
	ok(st.UIOpen, "a Casa window is open (needed for agent board tools)")
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
			ok(false, fmt.Sprintf("partner %s not connected (is their Casa running?)", p.Name))
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
	fmt.Println(`{"ok":true,"note":"UI rebuilt into cmd/casa/ui"}`)
}
