package pins

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OlivMartins/vigie/internal/filelock"
)

// FileVersion is the format version of pin files.
const FileVersion = 1

// Status is where a tool stands with respect to approval.
type Status string

// Tool statuses.
const (
	// StatusApproved: the definition seen last is the approved one.
	StatusApproved Status = "approved"
	// StatusChanged: an approved tool now has a different definition,
	// waiting for approval.
	StatusChanged Status = "changed"
	// StatusPending: a new tool waiting for its first approval.
	StatusPending Status = "pending"
	// StatusUnknown: the tool was never seen.
	StatusUnknown Status = "unknown"
)

// NewTools says what happens to a tool seen for the first time.
type NewTools string

// Policies for new tools.
const (
	// PinFirstSession approves the tools of the first session with a server
	// (its pin file was empty when the session began), and holds the tools
	// that appear in later sessions until someone approves them: a tool
	// added or renamed by an update is a new capability.
	PinFirstSession NewTools = "first-session"
	// PinOnFirstUse approves any new tool as soon as it is seen. Later
	// changes to its definition still need an approval.
	PinOnFirstUse NewTools = "pin"
	// HoldForApproval keeps every new tool pending until someone approves it.
	HoldForApproval NewTools = "block"
)

// Approvers recorded in pin files.
const (
	ApprovedOnFirstUse = "first-use"
	ApprovedByCommand  = "vigie pins approve"
)

// Snapshot is one version of a tool definition.
type Snapshot struct {
	Fingerprint string          `json:"fingerprint"`
	Definition  json.RawMessage `json:"definition"`
	SeenAt      time.Time       `json:"seen_at"`
	ApprovedAt  *time.Time      `json:"approved_at,omitempty"`
	ApprovedBy  string          `json:"approved_by,omitempty"`
}

// Entry holds the approved definition of a tool and, when it differs, the
// last definition seen that is waiting for approval.
type Entry struct {
	Approved *Snapshot `json:"approved,omitempty"`
	Pending  *Snapshot `json:"pending,omitempty"`
}

// Status reports the status of an entry.
func (e *Entry) Status() Status {
	switch {
	case e == nil:
		return StatusUnknown
	case e.Approved == nil:
		return StatusPending
	case e.Pending != nil:
		return StatusChanged
	default:
		return StatusApproved
	}
}

// File is the content of a pin file: one per upstream server, readable and
// diffable, meant to be reviewed and versioned like a lockfile.
type File struct {
	Version int               `json:"version"`
	Server  string            `json:"server"`
	Tools   map[string]*Entry `json:"tools"`
}

// Names returns the tool names in alphabetical order.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Tools))
	for n := range f.Tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Observed is a tool definition seen in a tools/list result.
type Observed struct {
	Name       string
	Definition json.RawMessage
}

// Observation is the outcome of comparing a definition with the pin file.
type Observation struct {
	Tool        string
	Status      Status
	New         bool // first time this tool is seen
	Fingerprint string
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidServerName reports whether name can name a pin file.
func ValidServerName(name string) bool { return serverName.MatchString(name) }

// Store reads and writes the pin file of one server. Several Vigie processes
// may share it: every change re-reads the file first and replaces it
// atomically.
type Store struct {
	path   string
	server string
	now    func() time.Time

	mu    sync.Mutex
	cache *File
	stamp stamp
	// firstSession is set by BeginSession when the pin file holds no tool.
	firstSession bool
	// fingerprints caches fingerprints by the SHA-256 of the raw definition:
	// servers send the same bytes on every tools/list.
	fingerprints map[[sha256.Size]byte]string
}

// maxCachedFingerprints bounds the cache; it is simply reset when full.
const maxCachedFingerprints = 4096

func (s *Store) fingerprint(raw json.RawMessage) (string, error) {
	key := sha256.Sum256(raw)
	if fp, ok := s.fingerprints[key]; ok {
		return fp, nil
	}
	fp, err := Fingerprint(raw)
	if err != nil {
		return "", err
	}
	if s.fingerprints == nil || len(s.fingerprints) >= maxCachedFingerprints {
		s.fingerprints = make(map[[sha256.Size]byte]string)
	}
	s.fingerprints[key] = fp
	return fp, nil
}

type stamp struct {
	mod  time.Time
	size int64
	ok   bool
}

// Open returns the store of server in dir. Nothing is written until a change
// is recorded.
func Open(dir, server string) (*Store, error) {
	if !ValidServerName(server) {
		return nil, fmt.Errorf("invalid server name %q: use letters, digits, '.', '_' or '-' (64 characters at most)", server)
	}
	return &Store{
		path:   filepath.Join(dir, server+".json"),
		server: server,
		now:    func() time.Time { return time.Now().UTC().Truncate(time.Second) },
	}, nil
}

// Path returns the location of the pin file.
func (s *Store) Path() string { return s.path }

// Load returns the current content of the pin file. The result must not be
// modified.
func (s *Store) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// BeginSession records whether this session is the first one with the
// server, that is whether its pin file holds no tool yet. Call it once, when
// the session starts.
func (s *Store) BeginSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	s.firstSession = len(f.Tools) == 0
	return nil
}

// FirstSession reports what BeginSession recorded.
func (s *Store) FirstSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstSession
}

// Approves reports whether a tool never seen before is approved on sight
// under the given policy.
func (s *Store) Approves(newTools NewTools) bool {
	switch newTools {
	case PinOnFirstUse:
		return true
	case PinFirstSession:
		return s.FirstSession()
	default:
		return false
	}
}

// Status returns the status of one tool.
func (s *Store) Status(tool string) (Status, error) {
	f, err := s.Load()
	if err != nil {
		return StatusUnknown, err
	}
	return f.Tools[tool].Status(), nil
}

// Observe compares the definitions of a tools/list result with the pin file,
// records what is new or changed, and returns the status of each tool.
func (s *Store) Observe(tools []Observed, newTools NewTools) ([]Observation, error) {
	approveNew := s.Approves(newTools)
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	f = f.clone()
	now := s.now()
	dirty := false
	out := make([]Observation, 0, len(tools))
	for _, t := range tools {
		fp, err := s.fingerprint(t.Definition)
		if err != nil {
			return nil, fmt.Errorf("fingerprinting tool %q: %w", t.Name, err)
		}
		snapshot := func() *Snapshot {
			return &Snapshot{Fingerprint: fp, Definition: compactJSON(t.Definition), SeenAt: now}
		}
		e := f.Tools[t.Name]
		obs := Observation{Tool: t.Name, Fingerprint: fp}
		switch {
		case e == nil:
			obs.New = true
			e = &Entry{}
			snap := snapshot()
			if approveNew {
				snap.ApprovedAt, snap.ApprovedBy = &now, ApprovedOnFirstUse
				e.Approved = snap
			} else {
				e.Pending = snap
			}
			f.Tools[t.Name] = e
			dirty = true
		case e.Approved != nil && e.Approved.Fingerprint == fp:
			if e.Pending != nil { // the server went back to the approved definition
				e.Pending = nil
				dirty = true
			}
		default:
			if e.Pending == nil || e.Pending.Fingerprint != fp {
				e.Pending = snapshot()
				dirty = true
			}
		}
		obs.Status = e.Status()
		out = append(out, obs)
	}
	if dirty {
		if err := s.save(f); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ApproveFingerprint approves the pending definition of a tool only if its
// fingerprint starts with the one given: what gets approved is what was
// reviewed, even if the server sent another definition in the meantime.
func (s *Store) ApproveFingerprint(tool, fingerprint string) error {
	want := strings.TrimPrefix(strings.ToLower(fingerprint), "sha256:")
	if len(want) < 12 {
		return fmt.Errorf("fingerprint %q is too short: give at least 12 hexadecimal characters", fingerprint)
	}
	_, err := s.update([]string{tool}, func(name string, e *Entry, now time.Time) error {
		if e.Pending == nil {
			return fmt.Errorf("tool %q has nothing waiting for approval", name)
		}
		if !strings.HasPrefix(strings.TrimPrefix(e.Pending.Fingerprint, "sha256:"), want) {
			return fmt.Errorf("tool %q changed again since the review: pending definition is %s, review it with vigie pins diff", name, e.Pending.Fingerprint)
		}
		snap := *e.Pending
		snap.ApprovedAt, snap.ApprovedBy = &now, ApprovedByCommand
		e.Approved, e.Pending = &snap, nil
		return nil
	}, nil)
	return err
}

// Approve approves the pending definition of each tool and returns the names
// approved. With no names, it approves every pending definition.
func (s *Store) Approve(tools ...string) ([]string, error) {
	return s.update(tools, func(name string, e *Entry, now time.Time) error {
		if e.Pending == nil {
			return fmt.Errorf("tool %q has nothing waiting for approval", name)
		}
		snap := *e.Pending
		snap.ApprovedAt, snap.ApprovedBy = &now, ApprovedByCommand
		e.Approved, e.Pending = &snap, nil
		return nil
	}, func(e *Entry) bool { return e.Pending != nil })
}

// Forget removes tools from the pin file: they will be treated as new the
// next time they are seen.
func (s *Store) Forget(tools ...string) ([]string, error) {
	if len(tools) == 0 {
		return nil, errors.New("name at least one tool to forget")
	}
	return s.update(tools, nil, nil)
}

// update applies apply to the named tools (or to those matching all when no
// name is given), deleting them when apply is nil.
func (s *Store) update(tools []string, apply func(string, *Entry, time.Time) error, all func(*Entry) bool) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	f = f.clone()
	if len(tools) == 0 {
		for _, name := range f.Names() {
			if all(f.Tools[name]) {
				tools = append(tools, name)
			}
		}
		if len(tools) == 0 {
			return nil, nil
		}
	}
	now := s.now()
	for _, name := range tools {
		e, ok := f.Tools[name]
		if !ok {
			return nil, fmt.Errorf("tool %q is not in %s", name, s.path)
		}
		if apply == nil {
			delete(f.Tools, name)
			continue
		}
		if err := apply(name, e, now); err != nil {
			return nil, err
		}
	}
	if err := s.save(f); err != nil {
		return nil, err
	}
	return tools, nil
}

// load returns the cached file, re-reading it when it changed on disk.
func (s *Store) load() (*File, error) {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.cache, s.stamp = &File{Version: FileVersion, Server: s.server, Tools: map[string]*Entry{}}, stamp{}
		return s.cache, nil
	}
	if err != nil {
		return nil, err
	}
	st := stamp{mod: info.ModTime(), size: info.Size(), ok: true}
	if s.cache != nil && s.stamp == st {
		return s.cache, nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if f.Version != FileVersion {
		return nil, fmt.Errorf("reading %s: unsupported version %d (expected %d)", s.path, f.Version, FileVersion)
	}
	if f.Server != s.server {
		return nil, fmt.Errorf("reading %s: file belongs to server %q, not %q", s.path, f.Server, s.server)
	}
	if f.Tools == nil {
		f.Tools = map[string]*Entry{}
	}
	s.cache, s.stamp = &f, st
	return s.cache, nil
}

// lockFile takes an exclusive lock shared by every process using this pin
// file, so that read-modify-write cycles do not overwrite each other.
func (s *Store) lockFile() (func(), error) { return filelock.Lock(s.path) }

// save writes f atomically: a reader sees the old file or the new one, never
// a partial write.
func (s *Store) save(f *File) error {
	// Descriptions are meant to be read in reviews: keep <, > and & as they are.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return err
	}
	data := buf.Bytes()
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+s.server+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.cache = f
	if info, err := os.Stat(s.path); err == nil {
		s.stamp = stamp{mod: info.ModTime(), size: info.Size(), ok: true}
	} else {
		s.stamp = stamp{}
	}
	return nil
}

func (f *File) clone() *File {
	c := &File{Version: f.Version, Server: f.Server, Tools: make(map[string]*Entry, len(f.Tools))}
	for name, e := range f.Tools {
		ce := &Entry{}
		if e.Approved != nil {
			a := *e.Approved
			ce.Approved = &a
		}
		if e.Pending != nil {
			p := *e.Pending
			ce.Pending = &p
		}
		c.Tools[name] = ce
	}
	return c
}

// compactJSON stores a definition in its canonical form, _meta excluded: what
// is stored is exactly what was fingerprinted and approved.
func compactJSON(raw json.RawMessage) json.RawMessage {
	canonical, err := Canonical(raw)
	if err != nil {
		return raw
	}
	return canonical
}
