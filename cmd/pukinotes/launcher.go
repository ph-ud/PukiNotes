package main

// The app shortcut. `pukinotes install` puts a real PukiNotes app where the
// human launches apps (macOS: Applications and, if asked, the Dock; Windows:
// Start menu and desktop; Linux: the app menu). Clicking it runs
// `pukinotes open`, which starts PukiNotes in the background if needed, shows
// the app window, and starts the human's own agent in a terminal window when
// no agent is listening, so it binds itself (see AGENTS.md, "Launch from the
// app shortcut").

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed icon/mac-1024.png
var iconMac1024 []byte

//go:embed icon/mac-256.png
var iconMac256 []byte

//go:embed icon/full-256.png
var iconFull256 []byte

// launcherConfig lives in ~/.pukinotes/launcher.json.
type launcherConfig struct {
	Repo          string    `json:"repo"`
	AgentCommand  string    `json:"agent_command,omitempty"` // e.g. claude "launch PukiNotes"
	Shortcuts     []string  `json:"shortcuts,omitempty"`
	AgentLaunched time.Time `json:"agent_launched,omitempty"`
}

func launcherPath() string { return filepath.Join(pukinotesHome(), "launcher.json") }

func loadLauncher() launcherConfig {
	var c launcherConfig
	if b, err := os.ReadFile(launcherPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveLauncher(c launcherConfig) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(launcherPath(), b, 0o600)
}

// cmdOpen is what the app shortcut runs.
func cmdOpen(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	noAgent := fs.Bool("no-agent", false, "don't start the agent, even if one is set up")
	fs.Parse(args)
	home := pukinotesHome()
	exitOn(os.MkdirAll(home, 0o700))

	s, err := readSession()
	running := err == nil
	if running {
		_, err = apiGet(s, "status")
		running = err == nil
	}
	if !running {
		s, err = startInBackground(home)
		exitOn(err)
	}
	how := openWindow(s.URL)

	agent := "not set up"
	lc := loadLauncher()
	if lc.AgentCommand != "" {
		agent = "skipped"
		if !*noAgent {
			agent = maybeStartAgent(s, &lc)
		}
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "window": how, "started": !running, "agent": agent})
	fmt.Println(string(out))
}

// startInBackground builds the current checkout and runs `pukinotes start`
// detached, the same way an update restarts PukiNotes.
func startInBackground(home string) (*session, error) {
	repo := findRepo()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if repo != "" {
		// `go run` deletes its binary when it exits, so build a lasting one (the updater uses the same path).
		built := filepath.Join(home, "bin", "pukinotes")
		if runtime.GOOS == "windows" {
			built += ".exe"
		}
		os.MkdirAll(filepath.Dir(built), 0o700)
		b := exec.Command("go", "build", "-o", built+".new", "./cmd/pukinotes")
		b.Dir = repo
		if out, err := b.CombinedOutput(); err == nil {
			os.Remove(built)
			if err := os.Rename(built+".new", built); err == nil {
				exe = built
			}
		} else {
			fmt.Fprintf(os.Stderr, "build failed, using this copy: %s\n", strings.TrimSpace(string(out)))
		}
	}
	os.Remove(filepath.Join(home, "session.json")) // stale; the new copy writes a fresh one
	logf, _ := os.OpenFile(filepath.Join(home, "pukinotes.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	c := exec.Command(exe, "start", "--no-open")
	if repo != "" {
		c.Dir = repo
	}
	if logf != nil {
		c.Stdout, c.Stderr = logf, logf
	}
	detach(c)
	if err := c.Start(); err != nil {
		return nil, err
	}
	go c.Wait()
	for i := 0; i < 600; i++ { // up to a minute: the first start may download the Go toolchain
		time.Sleep(100 * time.Millisecond)
		if s, err := readSession(); err == nil {
			if _, err := apiGet(s, "status"); err == nil {
				return s, nil
			}
		}
	}
	return nil, fmt.Errorf("PukiNotes didn't start; see %s", filepath.Join(home, "pukinotes.log"))
}

// maybeStartAgent opens the human's agent in a terminal window when no agent is listening.
func maybeStartAgent(s *session, lc *launcherConfig) string {
	b, err := apiGet(s, "status")
	if err != nil {
		return "unknown"
	}
	var st struct {
		Agent struct {
			Listening bool
		}
	}
	json.Unmarshal(b, &st)
	if st.Agent.Listening {
		return "listening"
	}
	if time.Since(lc.AgentLaunched) < 2*time.Minute { // started a moment ago, still binding
		return "starting"
	}
	repo := lc.Repo
	if repo == "" {
		repo = findRepo()
	}
	if err := runInTerminal(repo, lc.AgentCommand); err != nil {
		fmt.Fprintln(os.Stderr, "couldn't start the agent:", err)
		return "failed: " + err.Error()
	}
	lc.AgentLaunched = time.Now()
	saveLauncher(*lc)
	return "started"
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runInTerminal runs command in a new, visible terminal window, in dir.
func runInTerminal(dir, command string) error {
	switch runtime.GOOS {
	case "darwin":
		line := "cd " + shQuote(dir) + " && " + command
		as := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(line)
		return exec.Command("osascript",
			"-e", `tell application "Terminal" to do script "`+as+`"`,
			"-e", `tell application "Terminal" to activate`).Run()
	case "windows":
		ps := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		return exec.Command("powershell", "-NoProfile", "-Command",
			"Start-Process -WorkingDirectory "+ps(dir)+" -FilePath cmd -ArgumentList '/k', "+ps(command)).Run()
	default:
		line := "cd " + shQuote(dir) + " && " + command + "; exec \"${SHELL:-sh}\""
		for _, t := range [][]string{
			{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"},
			{"xfce4-terminal", "-x"}, {"kitty"}, {"alacritty", "-e"}, {"xterm", "-e"},
		} {
			if p, err := exec.LookPath(t[0]); err == nil {
				c := exec.Command(p, append(t[1:], "sh", "-c", line)...)
				detach(c)
				if err := c.Start(); err == nil {
					go c.Wait()
					return nil
				}
			}
		}
		return errors.New("no terminal app found")
	}
}

// cmdInstall creates (or with --uninstall removes) the PukiNotes app shortcut.
func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	agentCmd := fs.String("agent", "", `command that starts your agent and has it launch PukiNotes, e.g. claude "launch PukiNotes"`)
	dock := fs.Bool("dock", false, "macOS: also keep PukiNotes in the Dock")
	uninstall := fs.Bool("uninstall", false, "remove the app shortcut")
	fs.Parse(args)
	home := pukinotesHome()
	exitOn(os.MkdirAll(home, 0o700))
	lc := loadLauncher()

	if *uninstall {
		for _, p := range lc.Shortcuts {
			os.RemoveAll(p)
		}
		if runtime.GOOS == "darwin" {
			removeFromDock()
		}
		os.Remove(launcherPath())
		out, _ := json.Marshal(map[string]any{"ok": true, "removed": lc.Shortcuts})
		fmt.Println(string(out))
		return
	}

	repo := findRepo()
	if repo == "" {
		exitOn(errors.New("run this from the PukiNotes git folder: go run ./cmd/pukinotes install"))
	}
	goExe, err := userGo()
	exitOn(err)
	lc.Repo = repo
	if *agentCmd != "" {
		lc.AgentCommand = strings.TrimSpace(*agentCmd)
	}

	var made []string
	switch runtime.GOOS {
	case "darwin":
		made, err = installMac(repo, goExe, *dock)
	case "windows":
		made, err = installWindows(repo, goExe)
	default:
		made, err = installLinux(repo, goExe)
	}
	exitOn(err)
	lc.Shortcuts = made
	exitOn(saveLauncher(lc))
	window := "app window"
	if appBrowser() == "" {
		window = "browser tab (install Chrome, Edge or Brave for a separate app window)"
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "shortcuts": made, "agent_command": lc.AgentCommand, "window": window})
	fmt.Println(string(out))
}

// userGo finds the human's own go command. Under `go run`, PATH starts with the
// toolchain Go downloaded for go.mod; a shortcut should use the installed go,
// which keeps working when go.mod asks for a newer toolchain.
func userGo() (string, error) {
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if strings.Contains(filepath.ToSlash(p), "golang.org/toolchain@") {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
			return p, nil
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		return "", errors.New("go isn't installed (see AGENTS.md, Setup)")
	}
	return filepath.Abs(p)
}

func installMac(repo, goExe string, dock bool) ([]string, error) {
	dir := "/Applications"
	if f, err := os.CreateTemp(dir, ".pukinotes-*"); err == nil { // writable without sudo?
		f.Close()
		os.Remove(f.Name())
	} else {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, "Applications")
	}
	app := filepath.Join(dir, "PukiNotes.app")
	os.RemoveAll(app)
	for _, d := range []string{"Contents/MacOS", "Contents/Resources"} {
		if err := os.MkdirAll(filepath.Join(app, d), 0o755); err != nil {
			return nil, err
		}
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>PukiNotes</string>
	<key>CFBundleDisplayName</key><string>PukiNotes</string>
	<key>CFBundleIdentifier</key><string>app.pukinotes.launcher</string>
	<key>CFBundleExecutable</key><string>PukiNotes</string>
	<key>CFBundleIconFile</key><string>PukiNotes</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>` + version + `</string>
	<key>LSUIElement</key><true/>
</dict>
</plist>
`
	path := []string{filepath.Dir(goExe)}
	if g, err := exec.LookPath("git"); err == nil {
		path = append(path, filepath.Dir(g))
	}
	path = append(path, "/opt/homebrew/bin", "/usr/local/bin", "/usr/local/go/bin")
	script := "#!/bin/bash\n# PukiNotes launcher, made by `pukinotes install`. Run it again to refresh.\n" +
		"export PATH=" + shQuote(strings.Join(path, ":")) + ":\"$PATH\"\n" +
		envLine("PUKINOTES_HOME") + envLine("PUKINOTES_PORT") + envLine("PUKINOTES_BROWSER") + envLine("PUKINOTES_WINDOW") +
		"cd " + shQuote(repo) + " || exit 1\n" +
		"mkdir -p " + shQuote(pukinotesHome()) + "\n" +
		"exec " + shQuote(goExe) + " run ./cmd/pukinotes open >> " + shQuote(filepath.Join(pukinotesHome(), "launcher.log")) + " 2>&1\n"
	files := map[string][]byte{
		"Contents/Info.plist":               []byte(plist),
		"Contents/MacOS/PukiNotes":          []byte(script),
		"Contents/Resources/PukiNotes.icns": icns(),
	}
	for rel, b := range files {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "Contents/MacOS/") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(app, rel), b, mode); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	os.Chtimes(app, now, now) // nudge Finder to pick up the icon
	exec.Command("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", app).Run()
	if dock {
		addToDock(app)
	}
	return []string{app}, nil
}

func envLine(name string) string {
	if v := os.Getenv(name); v != "" {
		return "export " + name + "=" + shQuote(v) + "\n"
	}
	return ""
}

func addToDock(app string) {
	cur, _ := exec.Command("defaults", "read", "com.apple.dock", "persistent-apps").Output()
	if bytes.Contains(cur, []byte(app)) {
		return
	}
	tile := `<dict><key>tile-data</key><dict><key>file-data</key><dict><key>_CFURLString</key><string>` + app +
		`</string><key>_CFURLStringType</key><integer>0</integer></dict></dict></dict>`
	if exec.Command("defaults", "write", "com.apple.dock", "persistent-apps", "-array-add", tile).Run() == nil {
		exec.Command("killall", "Dock").Run()
	}
}

func removeFromDock() {
	// Rewrites the Dock list without PukiNotes, using the plist tools macOS ships with.
	script := `import plistlib,subprocess
p=subprocess.run(["defaults","export","com.apple.dock","-"],capture_output=True).stdout
d=plistlib.loads(p); a=d.get("persistent-apps",[])
k=[t for t in a if "PukiNotes.app" not in t.get("tile-data",{}).get("file-data",{}).get("_CFURLString","")]
if len(k)!=len(a):
  d["persistent-apps"]=k
  subprocess.run(["defaults","import","com.apple.dock","-"],input=plistlib.dumps(d))
  subprocess.run(["killall","Dock"])`
	exec.Command("/usr/bin/python3", "-c", script).Run()
}

// icns packs PNG images into a macOS icon file.
func icns() []byte {
	var body bytes.Buffer
	for _, e := range []struct {
		typ string
		png []byte
	}{{"ic10", iconMac1024}, {"ic08", iconMac256}} {
		body.WriteString(e.typ)
		binary.Write(&body, binary.BigEndian, uint32(8+len(e.png)))
		body.Write(e.png)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// ico wraps a 256×256 PNG in a Windows icon file.
func ico(png []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1})              // reserved, type icon, 1 image
	b.Write([]byte{0, 0, 0, 0})                                           // 256×256 (0 means 256), no palette
	binary.Write(&b, binary.LittleEndian, []uint16{1, 32})                // planes, bits per pixel
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(png)), 22}) // size, offset
	b.Write(png)
	return b.Bytes()
}

func installWindows(repo, goExe string) ([]string, error) {
	icon := filepath.Join(pukinotesHome(), "PukiNotes.ico")
	if err := os.WriteFile(icon, ico(iconFull256), 0o644); err != nil {
		return nil, err
	}
	ps := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	var made []string
	for _, folder := range []string{"Programs", "Desktop"} {
		var dir string
		if folder == "Programs" {
			dir = filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs")
		} else {
			out, err := exec.Command("powershell", "-NoProfile", "-Command", "[Environment]::GetFolderPath('Desktop')").Output()
			if err != nil {
				continue
			}
			dir = strings.TrimSpace(string(out))
		}
		lnk := filepath.Join(dir, "PukiNotes.lnk")
		cmd := "$s=(New-Object -ComObject WScript.Shell).CreateShortcut(" + ps(lnk) + ");" +
			"$s.TargetPath=" + ps(goExe) + ";$s.Arguments='run ./cmd/pukinotes open';" +
			"$s.WorkingDirectory=" + ps(repo) + ";$s.IconLocation=" + ps(icon) + ";" +
			"$s.WindowStyle=7;$s.Description='PukiNotes';$s.Save()"
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", cmd).CombinedOutput(); err != nil {
			return made, fmt.Errorf("couldn't make %s: %s", lnk, strings.TrimSpace(string(out)))
		}
		made = append(made, lnk)
	}
	return made, nil
}

func installLinux(repo, goExe string) ([]string, error) {
	icon := filepath.Join(pukinotesHome(), "PukiNotes.png")
	if err := os.WriteFile(icon, iconFull256, 0o644); err != nil {
		return nil, err
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		h, _ := os.UserHomeDir()
		dataHome = filepath.Join(h, ".local", "share")
	}
	dir := filepath.Join(dataHome, "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	desktop := filepath.Join(dir, "pukinotes.desktop")
	q := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(s) + `"`
	}
	entry := "[Desktop Entry]\nType=Application\nName=PukiNotes\nComment=A cozy whiteboard for two\n" +
		"Exec=" + q(goExe) + " run ./cmd/pukinotes open\nPath=" + repo + "\nIcon=" + icon +
		"\nTerminal=false\nCategories=Office;\nStartupWMClass=PukiNotes\n"
	if err := os.WriteFile(desktop, []byte(entry), 0o755); err != nil {
		return nil, err
	}
	return []string{desktop}, nil
}
