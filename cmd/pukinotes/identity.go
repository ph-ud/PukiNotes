package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Identity is this computer's PukiNotes identity. It never leaves this machine,
// except for the public parts that go into invites.
type Identity struct {
	mu        sync.Mutex           `json:"-"`
	path      string               `json:"-"`
	UserID    string               `json:"user_id"`
	Name      string               `json:"name"`
	Color     string               `json:"color"`
	ServerKey key.NodePrivate      `json:"server_key"`
	PSK       tailcat.PresharedKey `json:"psk"`
	ClientKey key.NodePrivate      `json:"client_key"`
	// Relay is the tailcat relay this copy settled on the first time.
	// Keeping it keeps our tailcat address the same across restarts, so a
	// partner whose copy was also restarted (or a fresh cloud sandbox) can
	// still find us.
	Relay *tailcfg.DERPRegion `json:"relay,omitempty"`
}

// Config holds optional settings from <home>/config.json.
type Config struct {
	// DERPMapURL points at your own relay map, if you run your own relay.
	DERPMapURL string `json:"derp_map_url,omitempty"`
}

var palette = []string{"#d4694a", "#7a9a6b", "#5b7fa6", "#c28a2c", "#b0607a", "#4f8f8a"}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func loadIdentity(home string) (*Identity, error) {
	p := filepath.Join(home, "identity.json")
	id := &Identity{path: p}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, id); err != nil {
			return nil, err
		}
		id.path = p
		return id, nil
	}
	id.UserID = "u" + randHex(8)
	id.Name = defaultName()
	id.Color = palette[int(id.UserID[1])%len(palette)]
	id.ServerKey = key.NewNode()
	id.PSK = tailcat.NewPresharedKey()
	id.ClientKey = key.NewNode()
	return id, id.save()
}

func defaultName() string {
	if n := strings.TrimSpace(os.Getenv("PUKINOTES_NAME")); n != "" {
		return n
	}
	if headlessWanted() { // a cloud sandbox: "root" isn't a name
		if u, err := user.Current(); err != nil || u.Username == "root" {
			return "Cloud"
		}
	}
	if u, err := user.Current(); err == nil {
		n := u.Name
		if n == "" {
			n = u.Username
		}
		if i := strings.LastIndexAny(n, `\`); i >= 0 {
			n = n[i+1:]
		}
		if f := strings.Fields(n); len(f) > 0 {
			n = f[0]
		}
		if n != "" && n != "root" {
			return strings.ToUpper(n[:1]) + n[1:]
		}
	}
	return "Me"
}

func (id *Identity) save() error {
	b, _ := json.MarshalIndent(id, "", "  ")
	tmp := id.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, id.path)
}

func (id *Identity) SavedRelay() *tailcfg.DERPRegion {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.Relay
}

func (id *Identity) SetRelay(r *tailcfg.DERPRegion) {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.Relay = r
	id.save()
}

func (id *Identity) Profile() Person {
	id.mu.Lock()
	defer id.mu.Unlock()
	return Person{ID: id.UserID, Name: id.Name, Color: id.Color}
}

func (id *Identity) SetProfile(name, color string) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if name = strings.TrimSpace(name); name != "" {
		if len(name) > 40 {
			name = name[:40]
		}
		id.Name = name
	}
	if color != "" && len(color) <= 9 && strings.HasPrefix(color, "#") {
		id.Color = color
	}
	id.save()
}

type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func loadConfig(home string) Config {
	var c Config
	if b, err := os.ReadFile(filepath.Join(home, "config.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}
