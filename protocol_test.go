package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test executable also acts as a real helper for subprocess tests.
func TestMain(m *testing.M) {
	if os.Getenv("GOCACHE_TEST_HELPER") == "1" {
		if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			io.WriteString(os.Stderr, err.Error()+"\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProtocolStream(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("x"), []byte("xx"), bytes.Repeat([]byte{0, 255, 10}, 100000)} {
		t.Run(humanSize(int64(len(body))), func(t *testing.T) {
			c := testCache(t, t.TempDir(), 1<<20)
			a, o := sha256.Sum256([]byte("action")), sha256.Sum256(body)
			var in, out bytes.Buffer
			enc := json.NewEncoder(&in)
			enc.Encode(request{ID: 1, Command: "get", ActionID: a[:]})
			enc.Encode(request{ID: 2, Command: "put", ActionID: a[:], OutputID: o[:], BodySize: int64(len(body))})
			if len(body) > 0 {
				in.WriteByte('\n')
				enc.Encode(base64.StdEncoding.EncodeToString(body))
			}
			enc.Encode(request{ID: 3, Command: "get", ActionID: a[:]})
			enc.Encode(request{ID: 4, Command: "close"})
			if err := serve(c, &in, &out); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(&out)
			for id := int64(0); id <= 4; id++ {
				var res response
				if err := dec.Decode(&res); err != nil {
					t.Fatal(err)
				}
				if res.ID != id || res.Err != "" {
					t.Fatalf("response %d: %+v", id, res)
				}
				if id == 1 && !res.Miss {
					t.Fatal("expected initial miss")
				}
				if id == 3 && (res.Miss || !bytes.Equal(res.OutputID, o[:]) || res.Size != int64(len(body))) {
					t.Fatalf("incorrect cache hit: %+v", res)
				}
			}
			items, _ := os.ReadDir(filepath.Join(c.dir, "sessions"))
			if len(items) != 0 {
				t.Fatal("close leaked session")
			}
		})
	}
}

func TestKilledHelperRecovery(t *testing.T) {
	dir := t.TempDir()
	bin, _ := os.Executable()
	cmd := exec.Command(bin, "-dir", dir)
	cmd.Env = append(os.Environ(), "GOCACHE_TEST_HELPER=1")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	dec := json.NewDecoder(out)
	var res response
	if err := dec.Decode(&res); err != nil {
		t.Fatal(err)
	}
	body := []byte("crash recovery")
	a, o := sha256.Sum256([]byte("action")), sha256.Sum256(body)
	enc := json.NewEncoder(in)
	enc.Encode(request{ID: 1, Command: "put", ActionID: a[:], OutputID: o[:], BodySize: int64(len(body))})
	enc.Encode(base64.StdEncoding.EncodeToString(body))
	if err := dec.Decode(&res); err != nil || res.Err != "" {
		t.Fatalf("put: %+v %v %s", res, err, stderr.String())
	}
	oldSessions, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil || len(oldSessions) != 1 {
		t.Fatalf("expected one active session: %v", err)
	}
	oldPath := filepath.Join(dir, "sessions", oldSessions[0].Name())
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	c := testCache(t, dir, 1<<20)
	s := testSession(t, c)
	defer s.close()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("abandoned session not reaped")
	}
	res, err = s.get(a[:])
	if err != nil || res.Miss {
		t.Fatalf("committed cache entry lost after kill: %+v %v", res, err)
	}
}

func TestGoToolIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("real Go subprocesses")
	}
	bin, _ := os.Executable()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache with spaces")
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":       "module example.com/fixture\n\ngo 1.24\n",
		"main.go":      "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"cache works\") }\n",
		"main_test.go": "package main\nimport \"testing\"\nfunc TestWorks(t *testing.T) { main() }\n",
	} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prog := "\"" + bin + "\" -dir \"" + cacheDir + "\" -max-size 1GiB"
	runGo := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOCACHE_TEST_HELPER=1", "GOCACHEPROG="+prog, "GOCACHE="+filepath.Join(dir, "go-fallback"), "GOWORK=off", "GOFLAGS=")
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, data)
		}
		return string(data)
	}
	runGo("build", "-o", filepath.Join(dir, "fixture"), ".")
	// Export paths must survive helper exit for tools that load Go metadata.
	export := strings.TrimSpace(runGo("list", "-export", "-f", "{{.Export}}", "."))
	if _, err := os.Stat(export); err != nil {
		t.Fatalf("export path vanished after go list exited: %v", err)
	}
	output := runGo("build", "-x", "-o", filepath.Join(dir, "fixture2"), ".")
	if strings.Contains(output, "/compile ") {
		t.Fatalf("warm build recompiled:\n%s", output)
	}
	for i := 0; i < 2; i++ {
		if output := runGo("run", "."); output != "cache works\n" {
			t.Fatalf("go run: %s", output)
		}
	}
	for i := 0; i < 2; i++ {
		output = runGo("test", ".")
	}
	if !strings.Contains(output, "(cached)") {
		t.Fatalf("test result was not cached: %s", output)
	}
	cmd := exec.Command(filepath.Join(dir, "fixture2"))
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != "cache works\n" {
		t.Fatalf("cached executable: %s %v", output, err)
	}
	items, err := os.ReadDir(filepath.Join(cacheDir, "sessions"))
	if err != nil || len(items) != 0 {
		t.Fatalf("sessions leaked: %d %v", len(items), err)
	}
	// Start a pair of independent Go commands against the same warm cache.
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := 0; i < 2; i++ {
		cmd := exec.Command("go", "test", "-count=1", ".")
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOCACHE_TEST_HELPER=1", "GOCACHEPROG="+prog, "GOCACHE="+filepath.Join(dir, "go-fallback"), "GOWORK=off", "GOFLAGS=")
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands, outputs = append(commands, cmd), append(outputs, &output)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("concurrent test failed: %v\n%s", err, outputs[i])
		}
	}
	// Force eviction of real compiler artifacts after the builds finish.
	c := testCache(t, cacheDir, 1<<20)
	if err := c.locked(func() error { _, err := c.trim(); return err }); err != nil {
		t.Fatal(err)
	}
	snap, err := c.scan()
	if err != nil || snap.bytes > c.max {
		t.Fatalf("real cache not bounded: %d %v", snap.bytes, err)
	}
}

func TestTruncatedBodyCleansSession(t *testing.T) {
	c := testCache(t, t.TempDir(), 1<<20)
	a := sha256.Sum256([]byte("action"))
	var in, out bytes.Buffer
	json.NewEncoder(&in).Encode(request{ID: 1, Command: "put", ActionID: a[:], OutputID: a[:], BodySize: 100})
	in.WriteString("\"YWJj")
	if err := serve(c, bufio.NewReader(&in), &out); err == nil {
		t.Fatal("accepted truncated body")
	}
	items, _ := os.ReadDir(filepath.Join(c.dir, "sessions"))
	if len(items) != 0 {
		t.Fatal("error leaked session")
	}
}
