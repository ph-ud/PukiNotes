package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed ui
var uiFS embed.FS

// Msg is the one message shape used between Go and the browser, and between
// two PukiNotes peers. Unused fields are omitted.
type Msg struct {
	T     string          `json:"t"`
	B     string          `json:"b,omitempty"`    // board id
	D     []byte          `json:"d,omitempty"`    // Yjs update / awareness bytes (base64 in JSON)
	ID    string          `json:"id,omitempty"`   // update id, request id, rpc id
	IDs   []string        `json:"ids,omitempty"`  // update ids
	Name  string          `json:"name,omitempty"` // board name, tool name
	Text  string          `json:"text,omitempty"`
	Code  string          `json:"code,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"` // structured payload
	Error string          `json:"error,omitempty"`
}

type App struct {
	home    string
	port    int
	token   string
	id      *Identity
	cfg     Config
	store   *Store
	peers   *PeerManager
	agent   *Agent
	updater *Updater
	srv     *http.Server
	stopped chan struct{}

	mu      sync.Mutex
	clients map[*uiClient]bool
	compactAsked map[string]time.Time
}

type uiClient struct {
	conn *websocket.Conn
	out  chan []byte
	seen time.Time
}

func (a *App) broadcast(m Msg, except *uiClient) {
	b, _ := json.Marshal(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	for c := range a.clients {
		if c == except {
			continue
		}
		select {
		case c.out <- b:
		default: // slow client; it will resync on reconnect
		}
	}
}

// anyClient returns the most recently connected browser, for agent tool calls.
func (a *App) anyClient() *uiClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	var best *uiClient
	for c := range a.clients {
		if best == nil || c.seen.After(best.seen) {
			best = c
		}
	}
	return best
}

func (c *uiClient) send(m Msg) {
	b, _ := json.Marshal(m)
	select {
	case c.out <- b:
	default:
	}
}

func jsonRaw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (a *App) boardList() []map[string]any {
	var out []map[string]any
	for _, b := range a.store.List() {
		out = append(out, map[string]any{"id": b.ID, "name": b.GetName(), "shared_with": a.peers.SharedWith(b.ID)})
	}
	return out
}

func (a *App) helloData() json.RawMessage {
	return jsonRaw(map[string]any{
		"me":      a.id.Profile(),
		"peers":   a.peers.Status(),
		"boards":  a.boardList(),
		"agent":   a.agent.Status(),
		"net":     a.peers.NetStatus(),
		"version": version,
		"update":  a.updater.Status(),
	})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	files := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/ws", a.handleWS)
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/mcp/") != a.token {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		a.agent.serveMCP(w, r)
	})
	mux.HandleFunc("/api/", a.handleAPI)
	return mux
}

// handleAPI serves the CLI: /api/<token>/<what>
func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/"), "/", 3)
	if len(parts) < 2 || parts[0] != a.token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	switch parts[1] {
	case "status":
		enc.Encode(map[string]any{"ok": true, "version": version, "me": a.id.Profile(), "boards": a.boardList(),
			"peers": a.peers.Status(), "net": a.peers.NetStatus(), "agent": a.agent.Status(), "ui_open": a.anyClient() != nil, "update": a.updater.Status()})
	case "update":
		// check, then update if main has something new (restarts PukiNotes)
		a.updater.Check()
		st := a.updater.Status()
		if n, _ := st["behind"].(int); n > 0 && st["error"] == "" {
			go a.updater.Apply()
			st["started"] = true
		}
		enc.Encode(st)
	case "shutdown":
		enc.Encode(map[string]any{"ok": true})
		go func() { time.Sleep(100 * time.Millisecond); a.Shutdown() }()
	case "bindcode":
		enc.Encode(map[string]any{"bind_code": a.agent.NewBindCode()})
	case "join":
		var req struct{ Code string }
		json.NewDecoder(r.Body).Decode(&req)
		board, err := a.peers.Join(r.Context(), req.Code)
		if err != nil {
			w.WriteHeader(400)
			enc.Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		enc.Encode(map[string]any{"ok": true, "board": board})
	case "tools":
		enc.Encode(toolDefs())
	case "tool":
		if len(parts) < 3 {
			http.Error(w, "missing tool", 400)
			return
		}
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		res, err := a.agent.Call(r.Context(), parts[2], args)
		if err != nil {
			w.WriteHeader(400)
			enc.Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		enc.Encode(res)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("t") != a.token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"127.0.0.1:*", "localhost:*"}})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	c := &uiClient{conn: conn, out: make(chan []byte, 4096), seen: time.Now()}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		for {
			select {
			case b := <-c.out:
				wctx, wc := context.WithTimeout(ctx, 30*time.Second)
				err := conn.Write(wctx, websocket.MessageText, b)
				wc()
				if err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	// hello + every board's updates
	c.send(Msg{T: "hello", Data: a.helloData()})
	for _, b := range a.store.List() {
		for _, d := range b.All() {
			c.send(Msg{T: "upd", B: b.ID, D: d})
		}
		c.send(Msg{T: "loaded", B: b.ID})
	}
	c.send(Msg{T: "ready"})
	a.mu.Lock()
	a.clients[c] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.clients, c)
		a.mu.Unlock()
		conn.CloseNow()
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m Msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		a.fromUI(c, m)
	}
}

func (a *App) fromUI(c *uiClient, m Msg) {
	switch m.T {
	case "upd":
		b := a.store.Get(m.B)
		if b == nil {
			return
		}
		id, isNew := b.Add(m.D)
		if isNew {
			a.broadcast(Msg{T: "upd", B: m.B, D: m.D}, c)
			a.peers.SendUpdate(m.B, id, m.D, "")
			a.maybeCompact(b, c)
		}
	case "aw":
		a.broadcast(Msg{T: "aw", B: m.B, D: m.D}, c)
		a.peers.SendAwareness(m.B, m.D, "")
	case "newboard":
		if _, created, err := a.store.Ensure(m.B, m.Name); err == nil && created {
			a.broadcast(Msg{T: "boards", Data: jsonRaw(a.boardList())}, nil)
		}
	case "archiveboard":
		if len(a.peers.SharedWith(m.B)) == 0 && a.store.Archive(m.B) == nil {
			a.broadcast(Msg{T: "boards", Data: jsonRaw(a.boardList())}, nil)
		}
	case "rename":
		if b := a.store.Get(m.B); b != nil {
			b.SetName(m.Name)
			a.broadcast(Msg{T: "boards", Data: jsonRaw(a.boardList())}, nil)
		}
	case "invite":
		code, err := a.peers.CreateInvite(m.B)
		c.send(Msg{T: "invite", B: m.B, Code: code, Error: errString(err)})
	case "join":
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			board, err := a.peers.Join(ctx, m.Code)
			c.send(Msg{T: "joined", B: board, Error: errString(err)})
		}()
	case "profile":
		var p Person
		json.Unmarshal(m.Data, &p)
		a.id.SetProfile(p.Name, p.Color)
		a.peers.SendProfile()
		a.broadcast(Msg{T: "hello", Data: a.helloData()}, nil)
	case "request":
		var r Request
		json.Unmarshal(m.Data, &r)
		a.agent.Enqueue(&r)
	case "rpcres":
		a.agent.resolve(m.ID, m.Data, m.Error)
	case "compact":
		if b := a.store.Get(m.B); b != nil && len(m.D) > 0 {
			if err := b.Compact(m.D); err != nil {
				log.Printf("compact %s: %v", m.B, err)
			}
		}
	case "update":
		go a.updater.Apply()
	case "checkupdate":
		go a.updater.Check()
	case "ping":
		a.mu.Lock()
		c.seen = time.Now()
		a.mu.Unlock()
	}
}

// maybeCompact asks a browser to send a full snapshot once a board's log gets long.
func (a *App) maybeCompact(b *Board, c *uiClient) {
	if b.Count() < 400 {
		return
	}
	a.mu.Lock()
	last := a.compactAsked[b.ID]
	if time.Since(last) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.compactAsked[b.ID] = time.Now()
	a.mu.Unlock()
	c.send(Msg{T: "compact", B: b.ID})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *App) Serve(ln net.Listener) error {
	a.srv = &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	return a.srv.Serve(ln)
}

func (a *App) Shutdown() {
	select {
	case <-a.stopped:
		return
	default:
	}
	close(a.stopped)
	a.peers.Close()
	if a.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		a.srv.Shutdown(ctx)
	}
}
