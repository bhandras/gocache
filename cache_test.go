package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCache(t *testing.T, dir string, max int64) *cache {
	t.Helper()
	c, err := openCache(dir, max, time.Hour, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.lock.Close() })
	return c
}

func testSession(t *testing.T, c *cache) *session {
	t.Helper()
	s, err := c.start()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func putBytes(t *testing.T, s *session, action string, body []byte) ([]byte, response) {
	t.Helper()
	a, o := sha256.Sum256([]byte(action)), sha256.Sum256(body)
	res, err := s.put(request{ActionID: a[:], OutputID: o[:], BodySize: int64(len(body))}, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return a[:], res
}

func TestPinnedFilesSurviveConcurrentTrim(t *testing.T) {
	dir := t.TempDir()
	a := testCache(t, dir, 1<<20)
	s1 := testSession(t, a)
	body := bytes.Repeat([]byte("x"), 4096)
	key, put := putBytes(t, s1, "compile", body)
	b := testCache(t, dir, 1<<20)
	s2 := testSession(t, b)
	get, err := s2.get(key)
	if err != nil || get.Miss {
		t.Fatalf("get: %+v, %v", get, err)
	}
	b.max = 100
	if err := b.locked(func() error { _, err := b.trim(); return err }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{put.DiskPath, get.DiskPath} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("pinned file lost: %s: %v", path, err)
		}
	}
	if err := s1.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(get.DiskPath); err != nil {
		t.Fatalf("closing first build removed second build's file: %v", err)
	}
	if err := s2.close(); err != nil {
		t.Fatal(err)
	}
	stats, err := b.scan()
	if err != nil || stats.bytes > b.max || stats.pinned != 0 {
		t.Fatalf("not reclaimed after final build: %+v, %v", stats, err)
	}
}

func TestTrimEvictsOldestAndPreservesRecentHit(t *testing.T) {
	c := testCache(t, t.TempDir(), 1<<20)
	s := testSession(t, c)
	var keys [][]byte
	var objects []string
	for i := 0; i < 3; i++ {
		body := bytes.Repeat([]byte{byte(i)}, 2048)
		key, _ := putBytes(t, s, fmt.Sprint(i), body)
		o := sha256.Sum256(body)
		keys = append(keys, key)
		objects = append(objects, c.path("objects", o[:]))
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		for _, path := range []string{objects[i], c.path("actions", keys[i])} {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Keep one complete entry, including filesystem block allocation.
	objInfo, _ := os.Stat(objects[2])
	metaInfo, _ := os.Stat(c.path("actions", keys[2]))
	c.max = (diskSize(objInfo) + diskSize(metaInfo)) * 3 / 2
	if err := c.locked(func() error { _, err := c.trim(); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(objects[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest object not evicted: %v", err)
	}
	s = testSession(t, c)
	defer s.close()
	res, err := s.get(keys[2])
	if err != nil || res.Miss || res.DiskPath == "" {
		t.Fatalf("newest entry lost: %+v, %v", res, err)
	}
	stats, _ := c.scan()
	if stats.bytes > c.max {
		t.Fatalf("still over budget: %d", stats.bytes)
	}
}

func TestPutValidationAndDeduplication(t *testing.T) {
	c := testCache(t, t.TempDir(), 1<<20)
	s := testSession(t, c)
	defer s.close()
	body := []byte("shared artifact")
	key, first := putBytes(t, s, "first", body)
	_, second := putBytes(t, s, "second", body)
	if first.DiskPath != second.DiskPath {
		t.Fatal("same output was not deduplicated")
	}
	files, _ := os.ReadDir(filepath.Join(c.dir, "objects"))
	if len(files) != 1 {
		t.Fatalf("objects: %d", len(files))
	}
	o := sha256.Sum256(body)
	if _, err := s.put(request{ActionID: key, OutputID: o[:], BodySize: 3}, strings.NewReader("bad")); err == nil {
		t.Fatal("accepted incorrect digest")
	}
	res, err := s.get(key)
	if err != nil || res.Miss {
		t.Fatalf("failed write damaged prior entry: %+v %v", res, err)
	}
	got, _ := os.ReadFile(res.DiskPath)
	if !bytes.Equal(got, body) {
		t.Fatal("failed write changed cached body")
	}
}

func TestMissingObjectBecomesMiss(t *testing.T) {
	c := testCache(t, t.TempDir(), 1<<20)
	s := testSession(t, c)
	defer s.close()
	body := []byte("object")
	key, _ := putBytes(t, s, "action", body)
	o := sha256.Sum256(body)
	if err := os.Remove(c.path("objects", o[:])); err != nil {
		t.Fatal(err)
	}
	res, err := s.get(key)
	if err != nil || !res.Miss {
		t.Fatalf("dangling action: %+v, %v", res, err)
	}
}

func TestDirectoryOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep")
	if err := os.WriteFile(path, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openCache(dir, 100, time.Hour, io.Discard); err == nil {
		t.Fatal("accepted unrelated directory")
	}
	if data, _ := os.ReadFile(path); string(data) != "unrelated" {
		t.Fatal("modified unrelated file")
	}
}
