// gocache is a size-limited local Go build cache for GOCACHEPROG.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gocache:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, stderr io.Writer) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command != "serve" && command != "stats" && command != "trim" {
		return fmt.Errorf("unknown command %q; use serve, stats, or trim", command)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("gocache "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", filepath.Join(base, "gocache"), "cache directory (separate from GOCACHE)")
	limit := flags.String("max-size", "10GiB", "cache limit, e.g. 5GiB, 10GB, 500MiB")
	interval := flags.Duration("trim-interval", time.Minute, "minimum interval between automatic scans during a build")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	max, err := parseSize(*limit)
	if err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("trim-interval must be positive")
	}
	c, err := openCache(*dir, max, *interval, stderr)
	if err != nil {
		return err
	}
	defer c.lock.Close()
	if command == "serve" {
		return serve(c, in, out)
	}
	return c.locked(func() error {
		if command == "trim" {
			if err := c.reapSessions(); err != nil {
				return err
			}
			if _, err := c.trim(); err != nil {
				return err
			}
		}
		s, err := c.scan()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Directory: %s\nSize: %s (%d bytes)\nLimit: %s\nPinned: %s\nFiles: %d\n", c.dir, humanSize(s.bytes), s.bytes, humanSize(max), humanSize(s.pinned), len(s.files))
		if s.bytes > max {
			fmt.Fprintln(out, "Over limit: active builds keep their files until they finish.")
		}
		return nil
	})
}

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	units := []struct {
		suffix string
		factor int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1}}
	factor := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, factor = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.factor
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/factor {
		return 0, fmt.Errorf("invalid max-size; use a positive integer with B, KB, MB, GB, KiB, MiB, or GiB")
	}
	return n * factor, nil
}

func humanSize(n int64) string {
	for _, u := range []struct {
		name string
		n    int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= u.n {
			return fmt.Sprintf("%.2f %s", float64(n)/float64(u.n), u.name)
		}
	}
	return fmt.Sprintf("%d B", n)
}
