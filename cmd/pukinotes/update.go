package main

// Updating from inside the app: git pull of main, rebuild, restart.
// The new copy keeps the same port, token and agent binding, so the open
// window and the bound agent carry on after a short reconnect.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Updater struct {
	app  *App
	repo string // git checkout PukiNotes runs from ("" = updates unavailable)

	mu      sync.Mutex
	state   string // idle, checking, updating, restarting
	behind  int
	latest  string // subject of the newest commit on origin/main
	dirty   bool
	err     string
	checked time.Time
}

func newUpdater(a *App) *Updater { return &Updater{app: a, repo: findRepo(), state: "idle"} }

// findRepo walks up from the working directory to the PukiNotes git checkout.
func findRepo() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(b, []byte("module pukinotes")) {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				return dir
			}
			return ""
		}
		up := filepath.Dir(dir)
		if up == dir {
			return ""
		}
		dir = up
	}
}

func (u *Updater) git(args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", u.repo}, args...)...)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, errors.New(s)
	}
	return s, nil
}

func (u *Updater) Status() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := map[string]any{"available": u.repo != "", "state": u.state, "behind": u.behind, "latest": u.latest, "dirty": u.dirty, "error": u.err}
	if u.repo == "" {
		st["error"] = "updates need PukiNotes started from its git folder (go run ./cmd/pukinotes start)"
	}
	if !u.checked.IsZero() {
		st["checked"] = u.checked
	}
	return st
}

func (u *Updater) broadcast() {
	u.app.broadcast(Msg{T: "update", Data: jsonRaw(u.Status())}, nil)
}

func (u *Updater) set(f func()) {
	u.mu.Lock()
	f()
	u.mu.Unlock()
	u.broadcast()
}

// Loop checks for new versions now and then.
func (u *Updater) Loop() {
	if u.repo == "" {
		return
	}
	time.Sleep(5 * time.Second)
	for {
		u.Check()
		select {
		case <-u.app.stopped:
			return
		case <-time.After(3 * time.Hour):
		}
	}
}

func (u *Updater) Check() {
	if u.repo == "" {
		return
	}
	u.mu.Lock()
	if u.state != "idle" {
		u.mu.Unlock()
		return
	}
	u.state = "checking"
	u.mu.Unlock()
	u.broadcast()
	var behind int
	var latest string
	var dirty bool
	_, err := u.git("fetch", "--quiet", "origin", "main")
	if err == nil {
		var n string
		if n, err = u.git("rev-list", "--count", "HEAD..origin/main"); err == nil {
			behind, _ = strconv.Atoi(n)
			latest, _ = u.git("log", "-1", "--format=%s", "origin/main")
			st, _ := u.git("status", "--porcelain", "--untracked-files=no")
			dirty = st != ""
		}
	}
	u.set(func() {
		u.state, u.behind, u.latest, u.dirty, u.checked, u.err = "idle", behind, latest, dirty, time.Now(), ""
		if err != nil {
			u.err = "couldn't check for updates: " + err.Error()
		}
	})
}

// Apply pulls main, builds it, and restarts into the new version.
// If anything fails before the restart, the current version keeps running.
func (u *Updater) Apply(stash bool) {
	if u.repo == "" {
		return
	}
	u.mu.Lock()
	if u.state != "idle" {
		u.mu.Unlock()
		return
	}
	u.state, u.err = "updating", ""
	u.mu.Unlock()
	u.broadcast()
	fail := func(msg string) {
		u.set(func() { u.state, u.err = "idle", msg })
	}
	if st, _ := u.git("status", "--porcelain", "--untracked-files=no"); st != "" {
		if !stash {
			u.set(func() { u.state, u.dirty, u.err = "idle", true, "" })
			return
		}
		// Keep the local edits safe in git's stash; `git stash list` shows them, `git stash pop` brings them back.
		if out, err := u.git("stash", "push", "-m", "PukiNotes update "+time.Now().Format("2006-01-02 15:04")); err != nil {
			fail("couldn't put the local changes aside: " + out)
			return
		}
	}
	old, err := u.git("rev-parse", "HEAD")
	if err != nil {
		fail(err.Error())
		return
	}
	if out, err := u.git("pull", "--ff-only", "origin", "main"); err != nil {
		fail("git pull failed: " + out)
		return
	}
	exe := filepath.Join(u.app.home, "bin", "pukinotes")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	os.MkdirAll(filepath.Dir(exe), 0o700)
	b := exec.Command("go", "build", "-o", exe+".new", "./cmd/pukinotes")
	b.Dir = u.repo
	if out, err := b.CombinedOutput(); err != nil {
		u.git("reset", "--keep", old) // back to the version that works
		fail("the new version didn't build, so I kept this one: " + strings.TrimSpace(string(out)))
		return
	}
	os.Remove(exe)
	if err := os.Rename(exe+".new", exe); err != nil {
		fail(err.Error())
		return
	}
	if err := u.app.agent.saveQueue(); err != nil {
		fail("couldn't save your agent's open requests: " + err.Error())
		return
	}
	logf, _ := os.OpenFile(filepath.Join(u.app.home, "pukinotes.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	c := exec.Command(exe, "start", "--no-open", "--port", strconv.Itoa(u.app.port))
	c.Dir = u.repo
	c.Env = append(os.Environ(), "PUKINOTES_TOKEN="+u.app.token, "PUKINOTES_WAIT_PORT=1", "PUKINOTES_AGENT="+u.app.agent.carryOver())
	if logf != nil {
		c.Stdout, c.Stderr = logf, logf
	}
	detach(c)
	if err := c.Start(); err != nil {
		os.Remove(filepath.Join(u.app.home, "requests-carry.json"))
		fail("couldn't start the new version: " + err.Error())
		return
	}
	go c.Wait()
	u.set(func() { u.state, u.dirty = "restarting", false })
	fmt.Fprintf(os.Stderr, "updated: restarting as pid %d\n", c.Process.Pid)
	time.Sleep(400 * time.Millisecond)
	u.app.Shutdown()
}
