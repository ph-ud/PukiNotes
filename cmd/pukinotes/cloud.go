package main

// Running PukiNotes inside an agent's cloud sandbox (see CLOUD.md).
//
// A sandbox differs from a laptop in three ways, and each has a small answer here:
//   - No screen. PukiNotes keeps a headless browser window open by itself, so
//     the board tools work (PUKINOTES_WINDOW=headless forces it; it is the
//     default on Linux without DISPLAY or WAYLAND_DISPLAY).
//   - Only a web proxy out. tailcat then reaches its relays over WebSocket
//     (needs the ts_debug_websockets build tag; PUKINOTES_RELAY=websocket forces it).
//   - Nothing survives the session. `pukinotes state export` packs this copy's
//     identity and pairings into one line; PUKINOTES_STATE (or `state import`)
//     puts them back, so a new session reconnects to the same boards.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// headlessWanted reports whether the PukiNotes window should be a headless browser.
func headlessWanted() bool {
	switch os.Getenv("PUKINOTES_WINDOW") {
	case "headless":
		return true
	case "app", "browser", "none":
		return false
	}
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func behindProxy() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// setupRelayTransport picks how tailcat talks to its relays. It returns a
// problem to show the human (and doctor) when the relays can't work here.
func setupRelayTransport() string {
	mode := os.Getenv("PUKINOTES_RELAY")
	if mode == "derp" || (mode == "" && !behindProxy()) {
		return ""
	}
	if !wsRelays {
		return "this network only lets PukiNotes out through a web proxy, so it needs relays over WebSocket: " +
			"stop PukiNotes and start it again with GOFLAGS=-tags=ts_debug_websockets (see CLOUD.md)"
	}
	os.Setenv("TS_DEBUG_DERP_WS_CLIENT", "1")
	return ""
}

func chromeArgs(headless bool, url, profile string, extra ...string) []string {
	args := []string{"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--disable-features=Translate"}
	if headless {
		args = append(args, "--headless=new", "--disable-gpu", "--disable-dev-shm-usage", "--hide-scrollbars", "--mute-audio")
		if os.Geteuid() == 0 { // containers run as root, where Chromium's sandbox refuses to start
			args = append(args, "--no-sandbox")
		}
	}
	return append(append(args, extra...), url)
}

// keepHeadlessWindow keeps one headless PukiNotes window connected for as long
// as PukiNotes runs, and closes it when PukiNotes stops.
func (a *App) keepHeadlessWindow(url string) {
	exe := appBrowser()
	if exe == "" {
		log.Printf("pukinotes: no Chrome or Chromium found, so the board tools can't work. Install one, or set PUKINOTES_BROWSER")
		return
	}
	var cmd *exec.Cmd
	exited := make(chan struct{})
	missing := 0
	launch := func() {
		cmd = exec.Command(exe, chromeArgs(true, url+"&seed=0", filepath.Join(a.home, "window"), "--window-size=1440,920")...)
		if cmd.Start() != nil {
			cmd = nil
			return
		}
		ch := make(chan struct{})
		exited = ch
		go func(c *exec.Cmd) { c.Wait(); close(ch) }(cmd)
	}
	// A restored copy waits (up to 20 s) for its partner first, so the window
	// opens on the shared boards instead of making a new empty one.
	for i := 0; i < 20 && a.peers.waitingForFirstPeer(); i++ {
		time.Sleep(time.Second)
	}
	for {
		select {
		case <-a.stopped:
			if cmd != nil {
				cmd.Process.Kill()
			}
			return
		case <-time.After(2 * time.Second):
		}
		if a.anyClient() != nil {
			missing = 0
			continue
		}
		missing++
		running := cmd != nil
		if running {
			select {
			case <-exited:
				running = false
			default:
			}
		}
		// A fresh window gets 20 s to connect; after that (or if it died) start another.
		if running && missing < 10 {
			continue
		}
		if cmd != nil {
			cmd.Process.Kill()
			<-exited
		}
		launch()
		missing = 0
	}
}

// ---- screenshot ----

func cmdScreenshot(args []string) {
	board, out, w, h := "", "pukinotes.png", 1280, 860
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			exitOn(fmt.Errorf("%s needs a value", args[i]))
			return ""
		}
		switch args[i] {
		case "--board":
			board = next()
		case "--out", "-o":
			out = next()
		case "--size": // CSS pixels; the picture is twice that
			fmt.Sscanf(next(), "%dx%d", &w, &h)
		default:
			exitOn(fmt.Errorf("usage: pukinotes screenshot [--board ID] [--out file.png] [--size 1280x860]"))
		}
	}
	s := mustSession()
	if board == "" {
		st, err := apiGet(s, "status")
		exitOn(err)
		var v struct {
			Board string `json:"default_board"`
		}
		json.Unmarshal(st, &v)
		if v.Board == "" {
			exitOn(errors.New("there are no boards yet"))
		}
		board = v.Board
	}
	// Headless Chrome won't make a window narrower than about 500 px: smaller
	// sizes crop the page instead of laying it out for that width.
	w, h = max(w, 600), max(h, 500)
	exe := appBrowser()
	if exe == "" {
		exitOn(errors.New("no Chrome or Chromium found; set PUKINOTES_BROWSER"))
	}
	abs, err := filepath.Abs(out)
	exitOn(err)
	exitOn(os.MkdirAll(filepath.Dir(abs), 0o755))
	tmp, err := os.MkdirTemp("", "pukinotes-shot")
	exitOn(err)
	defer os.RemoveAll(tmp)
	url := s.URL + "&b=" + board + "&fit=1&snap=1"
	c := exec.Command(exe, chromeArgs(true, url, tmp, fmt.Sprintf("--window-size=%d,%d", w, h),
		"--force-device-scale-factor=2", "--virtual-time-budget=6000", "--screenshot="+abs)...)
	if b, err := c.CombinedOutput(); err != nil {
		exitOn(fmt.Errorf("the browser failed: %v: %s", err, lastLines(string(b), 3)))
	}
	if st, err := os.Stat(abs); err != nil || st.Size() == 0 {
		exitOn(errors.New("no screenshot was written"))
	}
	o, _ := json.Marshal(map[string]any{"ok": true, "path": abs, "board": board, "size": fmt.Sprintf("%dx%d", w*2, h*2)})
	fmt.Println(string(o))
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}

// ---- state export / import ----

const statePrefix = "pukinotes-state1-"

var stateFiles = []string{"identity.json", "peers.json"}

func exportState(home string) (string, error) {
	files := map[string]json.RawMessage{}
	for _, f := range stateFiles {
		b, err := os.ReadFile(filepath.Join(home, f))
		if err != nil {
			if f == "identity.json" {
				return "", errors.New("this PukiNotes has no identity yet: start it once first")
			}
			continue
		}
		files[f] = b
	}
	raw, _ := json.Marshal(files)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(raw)
	zw.Close()
	return statePrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// importState writes the identity and pairings from code into home. Existing
// files are only replaced with force.
func importState(home, code string, force bool) error {
	code = strings.Join(strings.Fields(code), "")
	raw, ok := strings.CutPrefix(code, statePrefix)
	if !ok {
		return errors.New("that isn't a PukiNotes state line (it should start with " + statePrefix + ")")
	}
	z, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return errors.New("the state line is damaged; copy it again")
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return errors.New("the state line is damaged; copy it again")
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return errors.New("the state line is damaged; copy it again")
	}
	var files map[string]json.RawMessage
	if err := json.Unmarshal(b, &files); err != nil || files["identity.json"] == nil {
		return errors.New("the state line is damaged; copy it again")
	}
	if _, err := os.Stat(filepath.Join(home, "identity.json")); err == nil && !force {
		return errors.New("this PukiNotes already has an identity; add --force to replace it")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	for _, f := range stateFiles {
		if v, ok := files[f]; ok {
			if err := os.WriteFile(filepath.Join(home, f), v, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreStateFromEnv brings back a saved identity on a fresh machine.
func restoreStateFromEnv(home string) {
	code := os.Getenv("PUKINOTES_STATE")
	if code == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(home, "identity.json")); err == nil {
		return
	}
	if err := importState(home, code, false); err != nil {
		exitOn(fmt.Errorf("PUKINOTES_STATE: %w", err))
	}
}

func cmdState(args []string) {
	home := pukinotesHome()
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "export":
		code, err := exportState(home)
		exitOn(err)
		fmt.Println(code)
	case "import":
		force := false
		var code string
		for _, a := range args {
			if a == "--force" {
				force = true
			} else {
				code += a
			}
		}
		if code == "" || code == "-" {
			b, _ := io.ReadAll(os.Stdin)
			code = string(b)
		}
		if s, err := readSession(); err == nil {
			if _, err := apiGet(s, "status"); err == nil {
				exitOn(errors.New("stop PukiNotes first (pukinotes stop), then import"))
			}
		}
		exitOn(importState(home, code, force))
		fmt.Println(`{"ok":true,"note":"identity and pairings restored; start PukiNotes and it reconnects"}`)
	default:
		exitOn(errors.New("usage: pukinotes state export | pukinotes state import <line> [--force]"))
	}
}
