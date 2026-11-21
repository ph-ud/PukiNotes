package main

// Peer-to-peer sync over tailcat.
//
// Every PukiNotes runs a tailcat server with a stable key, so it has a stable
// tailcat address. An invite code carries that address plus a one-time secret
// and the board id. When the partner joins, the two copies swap their own
// addresses and a long-lived pair secret over the encrypted tunnel, and the
// invite secret is thrown away. After that either side can dial the other.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

const pukinotesPort = 7000
const protoVersion = 1

type PeerInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Color   string   `json:"color"`
	Addr    string   `json:"addr"` // their tailcat server address
	Pair    string   `json:"pair"` // shared secret for reconnecting
	Boards  []string `json:"boards"`
	Version string   `json:"version,omitempty"` // their PukiNotes version, last time we spoke
}

type invite struct {
	Board   string    `json:"board"`
	Expires time.Time `json:"expires"`
}

type hello struct {
	T       string `json:"t"`
	Proto   int    `json:"proto"`
	Version string `json:"version"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	Addr    string `json:"addr"`
	Pair    string `json:"pair,omitempty"`
	Invite  string `json:"invite,omitempty"`
	Board   string `json:"board,omitempty"`
}

type welcome struct {
	T       string            `json:"t"`
	Error   string            `json:"error,omitempty"`
	Pair    string            `json:"pair,omitempty"`
	Boards  map[string]string `json:"boards,omitempty"` // id -> name
	Proto   int               `json:"proto"`
	Version string            `json:"version"`
}

type peerConn struct {
	peerID   string
	conn     net.Conn
	out      chan Msg
	outbound bool
	since    time.Time
	latency  time.Duration
	agent    json.RawMessage // their agent's status: {bound,name,listening,…}
	closed   chan struct{}
	once     sync.Once
}

func (pc *peerConn) close() {
	pc.once.Do(func() { close(pc.closed); pc.conn.Close() })
}

func (pc *peerConn) send(m Msg) bool {
	select {
	case pc.out <- m:
		return true
	case <-pc.closed:
		return false
	default:
		pc.close() // too far behind; reconnect will resync
		return false
	}
}

type PeerManager struct {
	app  *App
	path string

	mu       sync.Mutex
	srv      *tailcat.Server
	addr     string
	netErr   string
	known    map[string]*PeerInfo
	invites  map[string]invite
	conns    map[string]*peerConn
	clients  map[string]*tailcat.Client
	dialing  map[string]bool
	nextDial map[string]time.Time
	backoff  map[string]time.Duration
	region   *tailcfg.DERPRegion
	closed   bool
}

func newPeerManager(a *App) *PeerManager {
	pm := &PeerManager{app: a, path: filepath.Join(a.home, "peers.json"),
		known: map[string]*PeerInfo{}, invites: map[string]invite{}, conns: map[string]*peerConn{},
		clients: map[string]*tailcat.Client{}, dialing: map[string]bool{}, nextDial: map[string]time.Time{},
		backoff: map[string]time.Duration{}}
	var saved struct {
		Peers   []*PeerInfo       `json:"peers"`
		Invites map[string]invite `json:"invites"`
	}
	if b, err := os.ReadFile(pm.path); err == nil {
		json.Unmarshal(b, &saved)
	}
	for _, p := range saved.Peers {
		pm.known[p.ID] = p
	}
	for s, inv := range saved.Invites {
		if time.Now().Before(inv.Expires) {
			pm.invites[s] = inv
		}
	}
	return pm
}

func (pm *PeerManager) saveLocked() {
	var saved struct {
		Peers   []*PeerInfo       `json:"peers"`
		Invites map[string]invite `json:"invites"`
	}
	for _, p := range pm.known {
		saved.Peers = append(saved.Peers, p)
	}
	saved.Invites = pm.invites
	b, _ := json.MarshalIndent(saved, "", "  ")
	os.WriteFile(pm.path+".tmp", b, 0o600)
	os.Rename(pm.path+".tmp", pm.path)
}

func quietLogf(format string, args ...any) {
	if os.Getenv("PUKINOTES_DEBUG") != "" {
		log.Printf("[tailcat] "+format, args...)
	}
}

// Start brings up the tailcat server (retrying while offline) and the dialer loop.
func (pm *PeerManager) Start() {
	go func() {
		delay := 2 * time.Second
		for {
			if pm.isClosed() {
				return
			}
			err := pm.startServer()
			if err == nil {
				break
			}
			pm.mu.Lock()
			pm.netErr = err.Error()
			pm.mu.Unlock()
			pm.app.broadcast(Msg{T: "net", Data: jsonRaw(pm.NetStatus())}, nil)
			time.Sleep(delay)
			delay = min(delay*2, time.Minute)
		}
		pm.app.broadcast(Msg{T: "net", Data: jsonRaw(pm.NetStatus())}, nil)
		pm.dialLoop()
	}()
}

func (pm *PeerManager) startServer() error {
	id := pm.app.id
	s := &tailcat.Server{
		Key:          id.ServerKey,
		PresharedKey: id.PSK,
		Logf:         quietLogf,
		DERPMapURL:   pm.app.cfg.DERPMapURL,
		Region:       pm.region,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != pukinotesPort {
				return nil
			}
			return pm.handleInbound
		},
	}
	if err := s.Start(); err != nil {
		return err
	}
	pm.mu.Lock()
	pm.srv = s
	pm.addr = string(s.TailcatAddr())
	pm.netErr = ""
	pm.mu.Unlock()
	return nil
}

func (pm *PeerManager) isClosed() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.closed
}

func (pm *PeerManager) Close() {
	pm.mu.Lock()
	pm.closed = true
	conns := pm.conns
	pm.conns = map[string]*peerConn{}
	srv := pm.srv
	clients := pm.clients
	pm.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
	for _, c := range clients {
		c.Close()
	}
	if srv != nil {
		srv.Close()
	}
}

func (pm *PeerManager) NetStatus() map[string]any {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return map[string]any{"ready": pm.srv != nil, "error": pm.netErr}
}

func (pm *PeerManager) Status() []map[string]any {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	var out []map[string]any
	for _, p := range pm.known {
		st := map[string]any{"id": p.ID, "name": p.Name, "color": p.Color, "boards": p.Boards, "online": false, "version": p.Version}
		if c, ok := pm.conns[p.ID]; ok {
			st["online"] = true
			st["latency_ms"] = c.latency.Milliseconds()
			if len(c.agent) > 0 {
				st["agent"] = c.agent
			}
		}
		out = append(out, st)
	}
	return out
}

func (pm *PeerManager) SharedWith(board string) []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	var out []string
	for _, p := range pm.known {
		if slices.Contains(p.Boards, board) {
			out = append(out, p.ID)
		}
	}
	return out
}

func (pm *PeerManager) broadcastPeers() {
	pm.app.broadcast(Msg{T: "peers", Data: jsonRaw(pm.Status())}, nil)
}

// ---- invites ----

type inviteCode struct {
	B string `json:"b"` // board
	S string `json:"s"` // one-time secret
	A string `json:"a"` // inviter's tailcat address
	N string `json:"n"` // inviter's name
}

// CreateInvite returns the board's invite code. It's the same code every time
// (it can be used again, e.g. on a second computer), until renew makes a new
// one, which also cancels the old one.
func (pm *PeerManager) CreateInvite(board string, renew bool) (string, error) {
	if pm.app.store.Get(board) == nil {
		return "", errors.New("unknown board")
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.addr == "" {
		return "", errors.New("still connecting to the relay; try again in a few seconds")
	}
	secret := ""
	for sec, inv := range pm.invites {
		if inv.Board != board {
			continue
		}
		if renew || (!inv.Expires.IsZero() && time.Now().After(inv.Expires)) {
			delete(pm.invites, sec)
		} else if inv.Expires.IsZero() {
			secret = sec
		}
	}
	if secret == "" {
		secret = randHex(16)
		pm.invites[secret] = invite{Board: board} // no expiry
		pm.saveLocked()
	} else if renew {
		pm.saveLocked()
	}
	ic := inviteCode{B: board, S: secret, A: pm.addr, N: pm.app.id.Profile().Name}
	b, _ := json.Marshal(ic)
	return "pukinotes1-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func parseInvite(code string) (inviteCode, error) {
	var ic inviteCode
	code = strings.Join(strings.Fields(code), "")
	raw, ok := strings.CutPrefix(code, "pukinotes1-")
	if !ok {
		return ic, errors.New("that doesn't look like a PukiNotes invite code (it should start with pukinotes1-)")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return ic, errors.New("the invite code is damaged; copy it again")
	}
	if err := json.Unmarshal(b, &ic); err != nil || ic.B == "" || ic.S == "" || ic.A == "" {
		return ic, errors.New("the invite code is damaged; copy it again")
	}
	return ic, nil
}

// Join uses an invite code to pair with its sender and get the board.
func (pm *PeerManager) Join(ctx context.Context, code string) (string, error) {
	ic, err := parseInvite(code)
	if err != nil {
		return "", err
	}
	c := &tailcat.Client{Server: tailcat.Addr(ic.A), Key: pm.app.id.ClientKey, Logf: quietLogf, DERPMapURL: pm.app.cfg.DERPMapURL}
	conn, err := c.DialTCPPort(ctx, pukinotesPort)
	if err != nil {
		c.Close()
		return "", fmt.Errorf("couldn't reach %s's PukiNotes (is it open?): %w", ic.N, err)
	}
	pc, err := pm.handshakeOut(conn, hello{Invite: ic.S, Board: ic.B}, ic.A)
	if err != nil {
		conn.Close()
		c.Close()
		return "", err
	}
	pm.mu.Lock()
	if old := pm.clients[pc.peerID]; old != nil && old != c {
		go old.Close()
	}
	pm.clients[pc.peerID] = c
	pm.mu.Unlock()
	go pm.run(pc)
	return ic.B, nil
}

func (pm *PeerManager) myHello() hello {
	p := pm.app.id.Profile()
	pm.mu.Lock()
	addr := pm.addr
	pm.mu.Unlock()
	return hello{T: "hello", Proto: protoVersion, Version: version, ID: p.ID, Name: p.Name, Color: p.Color, Addr: addr}
}

func (pm *PeerManager) handshakeOut(conn net.Conn, h hello, theirAddr string) (*peerConn, error) {
	mine := pm.myHello()
	mine.Pair, mine.Invite, mine.Board = h.Pair, h.Invite, h.Board
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := writeFrame(conn, mine); err != nil {
		return nil, err
	}
	var their hello
	if err := readFrame(conn, &their); err != nil {
		return nil, err
	}
	var w welcome
	if err := readFrame(conn, &w); err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	if w.Error != "" {
		return nil, errors.New(w.Error)
	}
	if w.Proto != protoVersion {
		return nil, fmt.Errorf("%s runs a different PukiNotes version (%s, you have %s); both of you should git pull and restart", their.Name, w.Version, version)
	}
	pm.mu.Lock()
	p := pm.known[their.ID]
	if p == nil {
		p = &PeerInfo{ID: their.ID}
		pm.known[their.ID] = p
	}
	p.Name, p.Color, p.Addr, p.Version = their.Name, their.Color, theirAddr, their.Version
	if their.Addr != "" {
		p.Addr = their.Addr
	}
	if w.Pair != "" {
		p.Pair = w.Pair
	}
	for b := range w.Boards {
		if !slices.Contains(p.Boards, b) {
			p.Boards = append(p.Boards, b)
		}
	}
	pm.saveLocked()
	pm.mu.Unlock()
	for b, name := range w.Boards {
		if _, created, err := pm.app.store.Ensure(b, name); err == nil && created {
			pm.app.broadcast(Msg{T: "boards", Data: jsonRaw(pm.app.boardList())}, nil)
		}
	}
	return &peerConn{peerID: their.ID, conn: conn, out: make(chan Msg, 8192), outbound: true, since: time.Now(), closed: make(chan struct{})}, nil
}

func (pm *PeerManager) handleInbound(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var h hello
	if err := readFrame(conn, &h); err != nil || h.T != "hello" || h.ID == "" {
		conn.Close()
		return
	}
	mine := pm.myHello()
	writeFrame(conn, mine)
	fail := func(msg string) {
		writeFrame(conn, welcome{T: "welcome", Error: msg, Proto: protoVersion, Version: version})
		conn.Close()
	}
	if h.Proto != protoVersion {
		fail(fmt.Sprintf("%s runs a different PukiNotes version (%s, you have %s); both of you should git pull and restart", mine.Name, version, h.Version))
		return
	}
	pm.mu.Lock()
	p := pm.known[h.ID]
	w := welcome{T: "welcome", Boards: map[string]string{}, Proto: protoVersion, Version: version}
	switch {
	case h.Invite != "":
		inv, ok := pm.invites[h.Invite]
		if !ok || inv.Board != h.Board || (!inv.Expires.IsZero() && time.Now().After(inv.Expires)) {
			pm.mu.Unlock()
			fail("this invite code isn't valid anymore (a new one was made, or it expired); ask for the current code")
			return
		}
		if p == nil {
			p = &PeerInfo{ID: h.ID, Pair: randHex(24)}
			pm.known[h.ID] = p
		}
		if !slices.Contains(p.Boards, inv.Board) {
			p.Boards = append(p.Boards, inv.Board)
		}
		w.Pair = p.Pair
	case h.Pair != "" && p != nil && subtle.ConstantTimeCompare([]byte(h.Pair), []byte(p.Pair)) == 1:
	default:
		pm.mu.Unlock()
		fail("not paired: ask for an invite code")
		return
	}
	p.Name, p.Color, p.Version = h.Name, h.Color, h.Version
	if h.Addr != "" {
		p.Addr = h.Addr
	}
	boards := slices.Clone(p.Boards)
	pm.saveLocked()
	pm.mu.Unlock()
	for _, b := range boards {
		if bd := pm.app.store.Get(b); bd != nil {
			w.Boards[b] = bd.GetName()
		}
	}
	if err := writeFrame(conn, w); err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	pm.app.broadcast(Msg{T: "boards", Data: jsonRaw(pm.app.boardList())}, nil)
	pm.run(&peerConn{peerID: h.ID, conn: conn, out: make(chan Msg, 8192), since: time.Now(), closed: make(chan struct{})})
}

// register keeps one connection per peer. If both sides dialed at once, both
// keep the connection dialed by the side with the smaller user id.
func (pm *PeerManager) register(pc *peerConn) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.closed {
		return false
	}
	if old, ok := pm.conns[pc.peerID]; ok {
		select {
		case <-old.closed:
		default:
			me := pm.app.id.UserID
			dialerOf := func(c *peerConn) string {
				if c.outbound {
					return me
				}
				return c.peerID
			}
			if dialerOf(old) <= dialerOf(pc) && dialerOf(old) != dialerOf(pc) {
				return false
			}
			go old.close()
		}
	}
	pm.conns[pc.peerID] = pc
	pm.backoff[pc.peerID] = 0
	return true
}

func (pm *PeerManager) run(pc *peerConn) {
	if !pm.register(pc) {
		pc.close()
		return
	}
	defer func() {
		pc.close()
		pm.mu.Lock()
		if pm.conns[pc.peerID] == pc {
			delete(pm.conns, pc.peerID)
		}
		pm.mu.Unlock()
		pm.broadcastPeers()
	}()
	pm.broadcastPeers()
	// writer
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case m := <-pc.out:
				pc.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
				if err := writeFrame(pc.conn, m); err != nil {
					pc.close()
					return
				}
			case <-t.C:
				pc.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := writeFrame(pc.conn, Msg{T: "ping", Text: time.Now().Format(time.RFC3339Nano)}); err != nil {
					pc.close()
					return
				}
			case <-pc.closed:
				return
			}
		}
	}()
	// Tell them where to reach us next time (our relay address can change)
	// and who we are now (name or color may have changed while apart).
	pm.mu.Lock()
	myAddr := pm.addr
	pm.mu.Unlock()
	if myAddr != "" {
		pc.send(Msg{T: "addr", Text: myAddr})
	}
	pc.send(Msg{T: "profile", Data: jsonRaw(pm.app.id.Profile())})
	if pm.app.agent != nil {
		pc.send(Msg{T: "agentst", Data: jsonRaw(pm.app.agent.Status())})
	}
	// initial sync: tell them what we have for each shared board
	for _, b := range pm.boardsWith(pc.peerID) {
		if bd := pm.app.store.Get(b); bd != nil {
			pc.send(Msg{T: "have", B: b, IDs: bd.Have(), Name: bd.GetName()})
		}
	}
	for {
		pc.conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		var m Msg
		if err := readFrame(pc.conn, &m); err != nil {
			return
		}
		pm.handle(pc, m)
	}
}

func (pm *PeerManager) boardsWith(peerID string) []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if p := pm.known[peerID]; p != nil {
		return slices.Clone(p.Boards)
	}
	return nil
}

func (pm *PeerManager) shares(peerID, board string) bool {
	return slices.Contains(pm.boardsWith(peerID), board)
}

func (pm *PeerManager) handle(pc *peerConn, m Msg) {
	switch m.T {
	case "ping":
		pc.send(Msg{T: "pong", Text: m.Text})
		return
	case "pong":
		if t, err := time.Parse(time.RFC3339Nano, m.Text); err == nil {
			pm.mu.Lock()
			pc.latency = time.Since(t)
			pm.mu.Unlock()
			pm.broadcastPeers()
		}
		return
	case "addr":
		if m.Text != "" {
			pm.mu.Lock()
			if k := pm.known[pc.peerID]; k != nil && k.Addr != m.Text {
				k.Addr = m.Text
				pm.saveLocked()
			}
			pm.mu.Unlock()
		}
		return
	case "agentst": // older copies never send this, and ignore it (no board)
		pm.mu.Lock()
		pc.agent = append(json.RawMessage(nil), m.Data...)
		pm.mu.Unlock()
		pm.broadcastPeers()
		return
	case "profile":
		var p Person
		json.Unmarshal(m.Data, &p)
		pm.mu.Lock()
		if k := pm.known[pc.peerID]; k != nil && p.Name != "" {
			k.Name, k.Color = p.Name, p.Color
			pm.saveLocked()
		}
		pm.mu.Unlock()
		// Two people with the same color is confusing: the larger id moves over.
		if me := pm.app.id.Profile(); p.Color != "" && p.Color == me.Color && me.ID > pc.peerID {
			for i, c := range palette {
				if c == me.Color {
					pm.app.id.SetProfile("", palette[(i+1)%len(palette)])
					break
				}
			}
			pm.SendProfile()
			pm.app.broadcast(Msg{T: "hello", Data: pm.app.helloData()}, nil)
		}
		pm.broadcastPeers()
		return
	}
	// everything else is about a board, which must be shared with this peer
	if !pm.shares(pc.peerID, m.B) {
		return
	}
	bd := pm.app.store.Get(m.B)
	if bd == nil {
		var err error
		if bd, _, err = pm.app.store.Ensure(m.B, m.Name); err != nil {
			return
		}
		pm.app.broadcast(Msg{T: "boards", Data: jsonRaw(pm.app.boardList())}, nil)
	}
	switch m.T {
	case "have":
		if want := bd.Missing(m.IDs); len(want) > 0 {
			pc.send(Msg{T: "want", B: m.B, IDs: want})
		}
		// push what they don't list
		theirs := map[string]bool{}
		for _, id := range m.IDs {
			theirs[id] = true
		}
		var push []string
		for _, id := range bd.Have() {
			if !theirs[id] {
				push = append(push, id)
			}
		}
		for _, d := range bd.Get(push) {
			pc.send(Msg{T: "upd", B: m.B, D: d})
		}
	case "want":
		for _, d := range bd.Get(m.IDs) {
			pc.send(Msg{T: "upd", B: m.B, D: d})
		}
	case "upd":
		id, isNew := bd.Add(m.D)
		if isNew {
			pm.app.broadcast(Msg{T: "upd", B: m.B, D: m.D}, nil)
			pm.SendUpdate(m.B, id, m.D, pc.peerID)
		}
	case "aw":
		pm.app.broadcast(Msg{T: "aw", B: m.B, D: m.D}, nil)
	}
}

func (pm *PeerManager) sendToBoardPeers(board, except string, m Msg) {
	pm.mu.Lock()
	var targets []*peerConn
	for id, c := range pm.conns {
		if id == except {
			continue
		}
		if p := pm.known[id]; p != nil && slices.Contains(p.Boards, board) {
			targets = append(targets, c)
		}
	}
	pm.mu.Unlock()
	for _, c := range targets {
		c.send(m)
	}
}

func (pm *PeerManager) SendUpdate(board, id string, d []byte, except string) {
	pm.sendToBoardPeers(board, except, Msg{T: "upd", B: board, D: d})
}

func (pm *PeerManager) SendAwareness(board string, d []byte, except string) {
	pm.sendToBoardPeers(board, except, Msg{T: "aw", B: board, D: d})
}

func (pm *PeerManager) SendProfile() {
	p := pm.app.id.Profile()
	pm.mu.Lock()
	var cs []*peerConn
	for _, c := range pm.conns {
		cs = append(cs, c)
	}
	pm.mu.Unlock()
	for _, c := range cs {
		c.send(Msg{T: "profile", Data: jsonRaw(p)})
	}
}

// SendAgent tells every connected partner how this computer's agent is doing.
func (pm *PeerManager) SendAgent(st json.RawMessage) {
	pm.mu.Lock()
	var cs []*peerConn
	for _, c := range pm.conns {
		cs = append(cs, c)
	}
	pm.mu.Unlock()
	for _, c := range cs {
		c.send(Msg{T: "agentst", Data: st})
	}
}

// dialLoop keeps trying to reach known peers that aren't connected.
func (pm *PeerManager) dialLoop() {
	for !pm.isClosed() {
		pm.mu.Lock()
		var todo []*PeerInfo
		for id, p := range pm.known {
			if _, ok := pm.conns[id]; ok || pm.dialing[id] || p.Addr == "" || p.Pair == "" {
				continue
			}
			if time.Now().Before(pm.nextDial[id]) {
				continue
			}
			pm.dialing[id] = true
			cp := *p
			todo = append(todo, &cp)
		}
		pm.mu.Unlock()
		for _, p := range todo {
			go pm.dial(p)
		}
		time.Sleep(2 * time.Second)
	}
}

func (pm *PeerManager) dial(p *PeerInfo) {
	defer func() {
		pm.mu.Lock()
		delete(pm.dialing, p.ID)
		pm.mu.Unlock()
	}()
	fail := func() {
		pm.mu.Lock()
		b := pm.backoff[p.ID]
		b = min(max(b*2, 3*time.Second), 60*time.Second)
		pm.backoff[p.ID] = b
		pm.nextDial[p.ID] = time.Now().Add(b)
		pm.mu.Unlock()
	}
	pm.mu.Lock()
	c := pm.clients[p.ID]
	if c != nil && string(c.Server) != p.Addr {
		go c.Close()
		c = nil
	}
	if c == nil {
		c = &tailcat.Client{Server: tailcat.Addr(p.Addr), Key: pm.app.id.ClientKey, Logf: quietLogf, DERPMapURL: pm.app.cfg.DERPMapURL}
		pm.clients[p.ID] = c
	}
	pm.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	conn, err := c.DialTCPPort(ctx, pukinotesPort)
	if err != nil {
		fail()
		return
	}
	pc, err := pm.handshakeOut(conn, hello{Pair: p.Pair}, p.Addr)
	if err != nil {
		conn.Close()
		fail()
		return
	}
	go pm.run(pc)
}

// ---- framing: 4-byte length + JSON ----

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 64<<20 {
		return errors.New("frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
