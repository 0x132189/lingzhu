# lingzhu

Core of the lingzhu agent: a Go HTTP + SSE server that runs one model loop per session, with
durable SQLite events, a permission layer and OpenAI-compatible providers.

## Build

```sh
wails3 task build   # bin/lingzhu.exe, version from VERSION
wails3 task test    # go vet + go test
wails3 task run -- run "list the files here"
```

Artifacts stay out of the repository: `bin/` is build output, and runtime state lives in the user
profile.

## Run

```sh
lingzhu serve        # HTTP server on 127.0.0.1:8787
lingzhu run <prompt> # one turn in the terminal
lingzhu version
```

## Paths

```text
%APPDATA%/lingzhu/config.json       configuration (LINGZHU_CONFIG, project lingzhu.json)
%LOCALAPPDATA%/lingzhu/data         session databases; core.lock guards one process per directory
%LOCALAPPDATA%/lingzhu/cache/log    core.log
```

`LINGZHU_HOME` redirects data, state, cache and tmp under one directory for development and tests.
The desktop client ships this binary beside itself, so it needs no environment variable.

## Layout

```text
cmd/lingzhu           serve / run / version
internal/app          dependency wiring
internal/agent        model loop, retries, compaction
internal/server       REST + SSE
internal/config       config file, environment and defaults
internal/global       per-user directory roots
internal/datadir      exclusive data-directory lock
internal/session      SQLite event store and message rebuild
internal/provider     OpenAI-compatible catalog
internal/tool         built-in tools
internal/permission   allow / ask / deny rules
```
