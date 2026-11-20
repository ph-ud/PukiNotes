package main

// Board storage. PukiNotes's Go side treats Yjs updates as opaque blobs: each one is
// stored once, identified by its SHA-256. Syncing two copies is then just
// "send me the ids I don't have", and applying an update twice is harmless.

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type entry struct {
	id   [32]byte
	data []byte
}

type Board struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`

	dir     string
	mu      sync.Mutex
	known   map[[32]byte]bool // everything ever seen, including compacted ids
	entries []entry
	byID    map[[32]byte]int
	log     *os.File
}

type Store struct {
	dir    string
	mu     sync.Mutex
	boards map[string]*Board
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, boards: map[string]*Board{}}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		b, err := openBoard(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		s.boards[b.ID] = b
	}
	return s, nil
}

func (s *Store) Get(id string) *Board {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boards[id]
}

func (s *Store) List() []*Board {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Board, 0, len(s.boards))
	for _, b := range s.boards {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Ensure returns the board with id, creating it if needed.
func (s *Store) Ensure(id, name string) (*Board, bool, error) {
	if !validID(id) {
		return nil, false, errors.New("invalid board id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.boards[id]; ok {
		return b, false, nil
	}
	dir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	b := &Board{ID: id, Name: name, Created: time.Now(), dir: dir}
	if err := b.saveMeta(); err != nil {
		return nil, false, err
	}
	b2, err := openBoard(dir)
	if err != nil {
		return nil, false, err
	}
	s.boards[id] = b2
	return b2, true, nil
}

func validID(id string) bool {
	if len(id) < 4 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func openBoard(dir string) (*Board, error) {
	b := &Board{dir: dir, known: map[[32]byte]bool{}, byID: map[[32]byte]int{}}
	mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(mb, b); err != nil {
		return nil, err
	}
	// compacted ids
	if ab, err := os.ReadFile(filepath.Join(dir, "absorbed.bin")); err == nil {
		for i := 0; i+32 <= len(ab); i += 32 {
			var id [32]byte
			copy(id[:], ab[i:i+32])
			b.known[id] = true
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.bin"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	var good int64
	for {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			break
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			break // torn write at the end: drop it
		}
		good += 4 + int64(n)
		b.addLocked(data, false)
	}
	f.Truncate(good)
	f.Seek(good, io.SeekStart)
	b.log = f
	return b, nil
}

func (b *Board) saveMeta() error {
	mb, _ := json.MarshalIndent(b, "", "  ")
	tmp := filepath.Join(b.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, mb, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(b.dir, "meta.json"))
}

func (b *Board) SetName(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == "" || name == b.Name {
		return
	}
	b.Name = name
	b.saveMeta()
}

func (b *Board) GetName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Name
}

func hashOf(data []byte) [32]byte { return sha256.Sum256(data) }

func (b *Board) addLocked(data []byte, persist bool) ([32]byte, bool) {
	id := hashOf(data)
	if b.known[id] {
		return id, false
	}
	if persist && b.log != nil {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
		b.log.Write(append(hdr[:], data...))
		b.log.Sync()
	}
	b.known[id] = true
	b.byID[id] = len(b.entries)
	b.entries = append(b.entries, entry{id, data})
	return id, true
}

// Add stores an update. It reports whether the update was new.
func (b *Board) Add(data []byte) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, isNew := b.addLocked(data, true)
	return hex.EncodeToString(id[:]), isNew
}

func (b *Board) Have() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.entries))
	for i, e := range b.entries {
		out[i] = hex.EncodeToString(e.id[:])
	}
	return out
}

func (b *Board) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Missing returns which of ids we have never seen.
func (b *Board) Missing(ids []string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, h := range ids {
		var id [32]byte
		if n, err := hex.Decode(id[:], []byte(h)); err != nil || n != 32 {
			continue
		}
		if !b.known[id] {
			out = append(out, h)
		}
	}
	return out
}

// Get returns stored updates for the given ids (unknown ids are skipped).
func (b *Board) Get(ids []string) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out [][]byte
	for _, h := range ids {
		var id [32]byte
		if n, err := hex.Decode(id[:], []byte(h)); err != nil || n != 32 {
			continue
		}
		if i, ok := b.byID[id]; ok {
			out = append(out, b.entries[i].data)
		}
	}
	return out
}

func (b *Board) All() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, len(b.entries))
	for i, e := range b.entries {
		out[i] = e.data
	}
	return out
}

// Compact replaces the log with one snapshot (a full Yjs state update made by
// the UI). Old ids are remembered so we never ask a peer for them again.
func (b *Board) Compact(snapshot []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) < 2 {
		return nil
	}
	tmp := filepath.Join(b.dir, "log.bin.tmp")
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(snapshot)))
	if err := os.WriteFile(tmp, append(hdr[:], snapshot...), 0o600); err != nil {
		return err
	}
	af, err := os.OpenFile(filepath.Join(b.dir, "absorbed.bin"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, e := range b.entries {
		af.Write(e.id[:])
	}
	af.Sync()
	af.Close()
	b.log.Close()
	if err := os.Rename(tmp, filepath.Join(b.dir, "log.bin")); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(b.dir, "log.bin"), os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	b.log = f
	sid := hashOf(snapshot)
	b.entries = []entry{{sid, snapshot}}
	b.byID = map[[32]byte]int{sid: 0}
	b.known[sid] = true
	return nil
}

// Archive moves a board out of the way (into ../archive). Nothing is deleted.
func (s *Store) Archive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[id]
	if !ok {
		return errors.New("no such board")
	}
	b.mu.Lock()
	if b.log != nil {
		b.log.Close()
		b.log = nil
	}
	b.mu.Unlock()
	arch := filepath.Join(filepath.Dir(s.dir), "archive")
	if err := os.MkdirAll(arch, 0o700); err != nil {
		return err
	}
	if err := os.Rename(b.dir, filepath.Join(arch, id+"-"+time.Now().Format("20060102-150405"))); err != nil {
		return err
	}
	delete(s.boards, id)
	return nil
}
