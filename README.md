# gocache

A local Go build cache with a disk limit. Uses `GOCACHEPROG`; no daemon, cron
job, network service, or third-party dependency is required.

Requires Go 1.24 or newer. Supports macOS and Linux on a local filesystem with
hard links and advisory file locks.

## Install and enable

Install from GitHub:

```sh
mkdir -p "$HOME/.local/bin"
GOCACHEPROG= GOBIN="$HOME/.local/bin" go install github.com/bhandras/gocache@latest
go env -w "GOCACHEPROG=$HOME/.local/bin/gocache -max-size 10GiB"
```

Or build from a local checkout:

```sh
mkdir -p "$HOME/.local/bin"
GOCACHEPROG= go build -o "$HOME/.local/bin/gocache" .
go env -w "GOCACHEPROG=$HOME/.local/bin/gocache -max-size 10GiB"
```

This applies to future Go commands, including those started by editors.
An explicit `GOCACHEPROG` environment variable overrides the saved Go setting.
If the executable path contains spaces, quote the path inside the setting:

```sh
go env -w 'GOCACHEPROG="/path with spaces/gocache" -max-size 10GiB'
```

The first build starts with a cold cache. Existing Go build files stay in
`GOCACHE`; this tool uses its own directory. It refuses to initialize a
nonempty directory that it does not own.

## Usage

```sh
"$HOME/.local/bin/gocache" stats
"$HOME/.local/bin/gocache" trim
go env GOCACHEPROG
```

Change the limit:

```sh
go env -w "GOCACHEPROG=$HOME/.local/bin/gocache -max-size 5GiB"
"$HOME/.local/bin/gocache" trim -max-size 5GiB
```

Flags follow the subcommand. `serve` is the default subcommand. `stats` and
`trim` use their own flags; pass the same limit and directory as your saved
`GOCACHEPROG` when changing the defaults.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-max-size` | `10GiB` | Limit on allocated disk space for artifacts and action metadata |
| `-dir` | OS cache directory + `/gocache` | Dedicated cache directory |
| `-trim-interval` | `1m` | Minimum interval between maintenance scans while serving requests |

On macOS the default directory is `~/Library/Caches/gocache`. On Linux it
follows `os.UserCacheDir`, normally `$XDG_CACHE_HOME/gocache` or
`~/.cache/gocache`. Supported size units are B, KB, MB, GB, KiB, MiB, and GiB,
with positive integer values. No suffix means bytes.

Disable the helper:

```sh
go env -u GOCACHEPROG
```

For a single command, use `GOCACHEPROG= go build ./...`.

## Cleanup policy

Each Go command starts a helper. The helper scans at startup, after a minute
when another cache request arrives, and at shutdown. If the cache exceeds
the limit, it removes the oldest entries until usage reaches 90% of the limit
or only pinned artifacts remain. Hits update access times; object access
times are coarsened to one minute to reduce metadata writes.

The limit is **soft while builds are running**. The Go cache protocol requires
every returned file to remain available until the build closes the cache.
Each helper pins artifacts with hard links. Cleanup skips those artifacts,
including ones used by another helper. The last build to finish can reclaim
them. If overlapping builds continuously hold more than the limit, usage
remains above the limit until those builds release their files.

Usage counts allocated filesystem blocks for objects and action records,
counting shared artifacts once. Directory entries, lock files, and temporary
uploads are outside that number. Temporary uploads can consume additional
space while a request is being written.

Cache bodies are immutable and addressed by SHA-256. Writes stream to a
temporary file, verify the size and digest, publish the object, then atomically
replace the action record. A global file lock serializes shared mutations.
Body uploads happen outside that lock. Each helper holds a separate session
lock. The next helper or `trim` command removes abandoned sessions after a
crash; cleanup does not rely on process IDs or timeouts.

An action record whose object was evicted becomes a cache miss. Identical
outputs from different actions share one object. Export paths point to the
durable object, so consumers of `go list -export` can read them after the Go
command exits, until a later eviction.

The limit covers this helper's build cache. Module downloads, Go's separate
fuzz cache, the previous `GOCACHE` contents, and caches owned by other tools
are outside it. `go clean -cache` cleans Go's default disk cache; it does not
trim this helper's directory. Use `gocache trim` for this cache.

## Verification

```sh
GOCACHEPROG= go test -race -count=1 ./...
GOCACHEPROG= go vet ./...
```

Tests cover oldest-entry eviction, multiple active builds, pinned files,
deduplication, rejected bad writes, dangling action records, directory
ownership, empty and streamed protocol bodies, truncated input, killed-helper
recovery, and actual cold/warm Go builds and tests. The Go integration test also
checks exported paths, directories with spaces, concurrent Go commands, and
eviction of real compiler artifacts.

Protocol reference: [Go's GOCACHEPROG protocol](https://go.dev/src/cmd/go/internal/cacheprog/cacheprog.go).

## License

[MIT](LICENSE). Copyright (c) 2026 András Bánki-Horváth.
