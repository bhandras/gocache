package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const marker = "gocache local build cache v1\n"

type cache struct {
	dir      string
	max      int64
	interval time.Duration
	stderr   io.Writer
	lock     *os.File
}

type entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time
}

type session struct {
	cache    *cache
	dir      string
	lock     *os.File
	lastTrim time.Time
}

func openCache(dir string, max int64, interval time.Duration, stderr io.Writer) (*cache, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// A marker prevents accidentally trimming a user's unrelated directory.
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	c := &cache{dir: dir, max: max, interval: interval, stderr: stderr, lock: f}
	err = c.locked(func() error {
		path := filepath.Join(dir, "FORMAT")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			items, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, item := range items {
				if item.Name() != ".lock" {
					return errors.New("cache directory is not empty and has no gocache FORMAT marker; choose a dedicated directory")
				}
			}
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if string(data) != marker {
			return errors.New("unrecognized gocache FORMAT marker")
		}
		for _, name := range []string{"actions", "objects", "sessions"} {
			if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func (c *cache) locked(fn func() error) error {
	if err := flock(c.lock, false); err != nil {
		return err
	}
	defer funlock(c.lock)
	return fn()
}

func (c *cache) start() (*session, error) {
	s := &session{cache: c, lastTrim: time.Now()}
	err := c.locked(func() error {
		if err := c.reapSessions(); err != nil {
			return err
		}
		if _, err := c.trim(); err != nil {
			return err
		}
		dir, err := os.MkdirTemp(filepath.Join(c.dir, "sessions"), "build-")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			os.RemoveAll(dir)
			return err
		}
		if err := flock(f, false); err != nil {
			f.Close()
			os.RemoveAll(dir)
			return err
		}
		s.dir, s.lock = dir, f
		return nil
	})
	return s, err
}

// The global lock serializes session creation, reaping, and all shared mutations.
// A session holds its own lock for its whole lifetime, including between requests.
func (c *cache) reapSessions() error {
	items, err := os.ReadDir(filepath.Join(c.dir, "sessions"))
	if err != nil {
		return err
	}
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		dir := filepath.Join(c.dir, "sessions", item.Name())
		f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = flock(f, true)
		if busy(err) {
			f.Close()
			continue
		}
		if err != nil {
			f.Close()
			return err
		}
		err = os.RemoveAll(dir)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *session) close() error {
	return s.cache.locked(func() error {
		err := os.RemoveAll(s.dir)
		s.lock.Close()
		if err != nil {
			return err
		}
		if err := s.cache.reapSessions(); err != nil {
			return err
		}
		_, err = s.cache.trim()
		return err
	})
}

func (c *cache) path(kind string, id []byte) string {
	return filepath.Join(c.dir, kind, hex.EncodeToString(id))
}

func (s *session) pin(output []byte) (string, error) {
	path := filepath.Join(s.dir, hex.EncodeToString(output))
	object := s.cache.path("objects", output)
	if err := os.Link(object, path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	// Return the durable object path. Export consumers (go list -export)
	// can use it after this helper exits, until a later eviction.
	return object, nil
}

func (s *session) get(action []byte) (response, error) {
	var res response
	err := s.cache.locked(func() error {
		path := s.cache.path("actions", action)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			res.Miss = true
			return nil
		}
		if err != nil {
			return err
		}
		var e entry
		if err := json.Unmarshal(data, &e); err != nil || len(e.OutputID) != sha256.Size || e.Size < 0 {
			return fmt.Errorf("invalid cache entry %s", path)
		}
		obj := s.cache.path("objects", e.OutputID)
		info, err := os.Stat(obj)
		if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() != e.Size) {
			res.Miss = true
			return os.Remove(path)
		}
		if err != nil {
			return err
		}
		pin, err := s.pin(e.OutputID)
		if err != nil {
			return err
		}
		now := time.Now()
		// Coarsened access times avoid metadata writes on every tiny lookup.
		if now.Sub(info.ModTime()) > time.Minute {
			if err := os.Chtimes(obj, now, now); err != nil {
				return err
			}
		}
		if err := os.Chtimes(path, now, now); err != nil {
			return err
		}
		res = response{OutputID: e.OutputID, Size: e.Size, Time: &e.Time, DiskPath: pin}
		return s.maybeTrim()
	})
	return res, err
}

func (s *session) put(req request, body io.Reader) (response, error) {
	var res response
	// Stream outside the shared lock. Temporary files belong to the session,
	// so another process can recover them if this helper is killed.
	f, err := os.CreateTemp(s.dir, "put-")
	if err != nil {
		return res, err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), body)
	closeErr := f.Close()
	if err != nil {
		return res, err
	}
	if closeErr != nil {
		return res, closeErr
	}
	if n != req.BodySize || !equalID(h.Sum(nil), req.OutputID) {
		return res, errors.New("body size or SHA-256 does not match put request")
	}
	err = s.cache.locked(func() error {
		obj := s.cache.path("objects", req.OutputID)
		// Link publishes a complete immutable object without overwriting pins.
		if err := os.Link(f.Name(), obj); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Remove(f.Name()); err != nil {
			return err
		}
		now := time.Now()
		if err := os.Chtimes(obj, now, now); err != nil {
			return err
		}
		e := entry{OutputID: req.OutputID, Size: n, Time: now}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		// Rename commits the action only after its object is available.
		tmp := filepath.Join(s.dir, "action-tmp")
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, s.cache.path("actions", req.ActionID)); err != nil {
			return err
		}
		pin, err := s.pin(req.OutputID)
		if err != nil {
			return err
		}
		res.DiskPath = pin
		return s.maybeTrim()
	})
	return res, err
}

func equalID(a, b []byte) bool { return string(a) == string(b) }

func (s *session) maybeTrim() error {
	if time.Since(s.lastTrim) < s.cache.interval {
		return nil
	}
	s.lastTrim = time.Now()
	if err := s.cache.reapSessions(); err != nil {
		return err
	}
	_, err := s.cache.trim()
	return err
}

type cacheFile struct {
	path   string
	size   int64
	mtime  time.Time
	pinned bool
}

type snapshot struct {
	bytes  int64
	pinned int64
	files  []cacheFile
}

func (c *cache) scan() (snapshot, error) {
	var s snapshot
	for _, kind := range []string{"actions", "objects"} {
		dir := filepath.Join(c.dir, kind)
		items, err := os.ReadDir(dir)
		if err != nil {
			return s, err
		}
		for _, item := range items {
			info, err := item.Info()
			if err != nil {
				return s, err
			}
			if !info.Mode().IsRegular() {
				return s, fmt.Errorf("unexpected non-regular cache file: %s", filepath.Join(dir, item.Name()))
			}
			pinned := kind == "objects" && links(info) > 1
			size := diskSize(info)
			s.bytes += size
			if pinned {
				s.pinned += size
			}
			s.files = append(s.files, cacheFile{filepath.Join(dir, item.Name()), size, info.ModTime(), pinned})
		}
	}
	return s, nil
}

// Call with the global lock held. Hard links keep every returned DiskPath
// valid for the lifetime of its build, even with simultaneous trim processes.
func (c *cache) trim() (int64, error) {
	s, err := c.scan()
	if err != nil || s.bytes <= c.max {
		return 0, err
	}
	sort.Slice(s.files, func(i, j int) bool { return s.files[i].mtime.Before(s.files[j].mtime) })
	target := c.max - c.max/10
	var freed int64
	for _, f := range s.files {
		if s.bytes-freed <= target {
			break
		}
		if f.pinned {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return freed, err
		}
		freed += f.size
	}
	if freed > 0 {
		fmt.Fprintf(c.stderr, "gocache: evicted %s; cache %s / %s\n", humanSize(freed), humanSize(s.bytes-freed), humanSize(c.max))
	}
	return freed, nil
}
