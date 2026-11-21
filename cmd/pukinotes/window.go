package main

// PukiNotes opens in its own app window: a Chromium-family browser (Chrome,
// Edge, Brave, Chromium) in --app mode, with no tabs, no address bar and no
// visible localhost link. It uses its own small browser profile in
// ~/.pukinotes/window, so it is a separate window from the human's browsing.
// If no such browser is installed, it falls back to a normal browser tab.
//
//	PUKINOTES_WINDOW=browser   always use a normal browser tab
//	PUKINOTES_BROWSER=/path    use this Chromium-family browser for the app window

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// openWindow shows PukiNotes at url. It reports which way it opened: "app" or "browser".
func openWindow(url string) string {
	if headlessWanted() {
		return "headless" // the running PukiNotes keeps its own headless window (cloud.go)
	}
	if os.Getenv("PUKINOTES_WINDOW") != "browser" {
		if exe := appBrowser(); exe != "" {
			c := exec.Command(exe,
				"--app="+url,
				"--user-data-dir="+filepath.Join(pukinotesHome(), "window"),
				"--no-first-run",
				"--no-default-browser-check",
				"--disable-features=Translate",
				"--window-size=1440,920",
				"--class=PukiNotes", // groups the window with the launcher on Linux
			)
			detach(c)
			if c.Start() == nil {
				go c.Wait()
				return "app"
			}
		}
	}
	openBrowser(url)
	return "browser"
}

// appBrowser finds a browser that can open a chromeless app window.
func appBrowser() string {
	if p := os.Getenv("PUKINOTES_BROWSER"); p != "" {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
			for _, app := range [][2]string{
				{"Google Chrome", "Google Chrome"},
				{"Microsoft Edge", "Microsoft Edge"},
				{"Brave Browser", "Brave Browser"},
				{"Chromium", "Chromium"},
				{"Vivaldi", "Vivaldi"},
			} {
				candidates = append(candidates, filepath.Join(dir, app[0]+".app", "Contents", "MacOS", app[1]))
			}
		}
	case "windows":
		var roots []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if v := os.Getenv(env); v != "" {
				roots = append(roots, v)
			}
		}
		// Edge first: it ships with Windows, so it is always there.
		for _, rel := range []string{
			`Microsoft\Edge\Application\msedge.exe`,
			`Google\Chrome\Application\chrome.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
			`Chromium\Application\chrome.exe`,
		} {
			for _, r := range roots {
				candidates = append(candidates, filepath.Join(r, rel))
			}
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "brave-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				return p
			}
		}
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
