# Usage

Tailge has two interfaces:

- `tailge` starts the interactive service workspace when attached to a TTY.
- Subcommands provide scriptable, JSON-capable operations.

Tailge observes local listeners and the local Tailscale client. It does not proxy application traffic or control services on remote machines.

## Requirements and installation

- Go 1.25.10 or newer to build from source.
- macOS or Linux for local TCP discovery.
- An installed, logged-in Tailscale client for exposure features.
- An interactive TTY for the workspace.

Install from the repository root:

```sh
make setup
```

This builds the latest source and installs `tailge` in `$HOME/.local/bin`. Add it to `PATH` when necessary:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Useful local build targets:

```sh
make build                 # build ./tailge
make tui                   # build and launch the workspace
make run ARGS="scan --json"
make help
```

`make cross-build` writes only Linux amd64 and Darwin arm64 artifacts under `dist/`. Run `make build` before manually testing `./tailge` after source changes.

## CLI

### Discover listeners

Discovery does not require Tailscale:

```sh
tailge scan
tailge scan --json
```

The scan reports address, port, process, PID when available, metadata quality, and network scope. Linux discovery uses `lsof` when available and falls back to `ss`.

### Check readiness and routes

```sh
tailge doctor --tailscale
tailge doctor --tailscale --json
tailge exposure status
tailge exposure status --json
```

Serve and Funnel readiness are reported separately. `read_only` is a deliberate fail-closed state: the daemon, identity, and connection may be ready while exact mutation capability remains unverified. Serve requires persisted compatibility evidence. Funnel is enabled only when the installed CLI supports exact per-listener operations; broad `funnel reset` support is not sufficient.

Tailscale status may describe the same endpoint in both Serve and Funnel data. Tailge classifies an endpoint as public only when its matching `AllowFunnel` entry is explicitly `true`; otherwise it remains a private Serve route. The hostname or URL alone does not identify the access scope.

### Probe compatibility (optional)

A probe uses a disposable loopback listener to verify exact Serve or Funnel support before managing a real service:

```sh
python3 -m http.server 39001 --bind 127.0.0.1

tailge doctor --tailscale \
  --probe serve \
  --target 127.0.0.1:39001 \
  --confirm-test-route
```

Tailge can create the temporary listener with `--self-test-listener`. Funnel probes additionally require `--confirm-public`. Probes use exact selectors, verify cleanup, and never target an application listener. Probing is optional; every real mutation performs fresh verification.

### Manage exposure

Use an exact `address:port`, or pass a port with `--address`:

```sh
# Private tailnet exposure
tailge exposure serve 127.0.0.1:3000
tailge exposure serve 3000 --address 127.0.0.1

# Public exposure; confirmation is mandatory
tailge exposure funnel 127.0.0.1:3000 --confirm-public

# Disable one exact route
tailge exposure disable 127.0.0.1:3000

# Select one route when both Serve and Funnel exist
tailge exposure disable 127.0.0.1:3000 --mode serve
```

Add `--json` to mutation commands for machine-readable responses. Routes with unknown or external ownership require `--confirm-external` before replacement or removal:

```sh
tailge exposure disable 127.0.0.1:3000 --confirm-external --json
```

Mutations are bounded, serialized per target, protected by fresh route fingerprints, and verified with a fresh provider read. Replacement is allowed only when the previous route has a deterministic selector that can restore it exactly if the new operation fails. A command that cannot prove its final state reports `unverified`, not success. Omitting `--mode` for an ambiguous disable remains fail-closed.

### Configure preferences

The TUI creates `~/.tailge/config.tailge` on first run. The directory is owner-only (`0700`) and the file is owner read/write (`0600`); no credentials are stored.

```sh
tailge config path
tailge config show
tailge config validate
tailge config set refresh_interval 10s
tailge config set operation_timeout 30s
tailge config set sort name
tailge config set color_theme dark
```

Supported settings also include `show_system_listeners` and `show_inactive_configured_ports`. `color_theme` accepts `auto` (the default), `dark`, and `light`. `config validate` checks an existing file without creating a missing one. Invalid or unsafe configuration is preserved and mutations fail closed.

In the TUI, `s`, `f`, and `d` open explicitly labeled Serve, Funnel, and Disabled previews; no provider mutation occurs until confirmation. `C` or `Ctrl-l` clears an accepted filter; `/` edits it and `Ctrl-u` clears the active search text.

### Shell completion

```sh
tailge completion bash > "$HOME/.local/share/bash-completion/completions/tailge"
tailge completion zsh > "$HOME/.zfunc/_tailge"
tailge completion fish > "$HOME/.config/fish/completions/tailge.fish"
```

Create the parent directory first when necessary, then reload the shell.

## Interactive workspace

Start it with:

```sh
tailge
```

The workspace uses Bubble Tea's alternate screen and raw keyboard mode only after TTY validation, and restores terminal state on exit. On terminals at least 100 columns by 24 rows it uses a roughly 40/60 service-list/detail split; smaller terminals collapse to one focused pane. The list is selected first, and refreshes preserve stable item identity.

### Shortcuts

| Key | Action |
|---|---|
| `j` / `k`, `n` / `N`, arrows | Navigate the list or scroll details |
| `Tab` / `Shift-Tab`, `h` / `l` | Change pane focus |
| `Enter` | Focus or expand details; never mutates |
| `gg`, `G`, `Home`, `End` | First or last item |
| `/` | Incremental search; `Enter` accepts, `Esc` cancels, `Ctrl-u` clears |
| `s` / `f` / `d` | Open Serve, Funnel, or Disable preview |
| `Space` | Open the exposure action selector |
| `v` | Toggle the current item |
| `V` | Enter or exit Vim-style visual-line selection |
| `r` / `R` | Refresh / fresh retry preview after failure |
| `C` / `Ctrl-l` | Clear the accepted filter |
| `c` | Open confirmed cancellation for an Applying operation |
| `o` | Open the observed exposure URL |
| `O` | Open `http://localhost:<port>/` for the selected listener |
| `y` | Copy the observed URL through terminal OSC 52 |
| `x` | Confirm termination of current local process(es) |
| `U` | Clear all selected items |
| `X` | Dismiss visible feedback |
| `:` | Open the command palette |
| `?` | Open scrollable help |
| `q` / `Ctrl-c` | Quit; guarded while Applying |

The command palette supports `:refresh`, `:retry`, `:serve 3000`, `:funnel 3000`, `:disable 3000`, `:sort port`, `:config show`, `:config set key value`, `:config validate`, and `:quit`.

The service list uses one `SELECT` column: `[ ]` is unselected, `[✓]` is selected, and `[V]` is in the active visual range. Listeners sharing a port collapse into one row and use the available local port as the target. During confirmed process termination, the active row shows `TERMINATING`; selected batches advance one process at a time. Exposure actions on multiple items run sequentially and report each result independently.

Action previews default to Cancel. Funnel displays a public-internet warning and requires moving focus to Confirm. Unknown, unavailable, stale, ambiguous, and read-only states block unsafe changes. The workspace remains usable when Tailscale or one data source is unavailable. The list groups local listeners, inactive configured routes, and unknown or unavailable rows; very small terminals show a resize notice instead of an overflowing modal.

### URLs, transport, and process actions

`o` opens an explicitly observed HTTP/HTTPS URL. For a Serve `tcp=` route without an observed URL, an explicit `o` may use the provider-reported Tailscale DNS name and exact listener port as an HTTP preview. This is a UI convenience only: it never changes route identity or mutation behavior. Funnel TCP without an observed URL remains `TCP-only` and cannot be copied.

`O` opens the local convenience URL `http://localhost:<port>/`. It is not evidence that a remote raw-TCP exposure speaks HTTP.

`y` copies an observed URL through OSC 52, so SSH, Mosh, and multiplexer sessions update the attached terminal client's clipboard. For Serve TCP without an observed URL it copies the same provider-DNS preview used by `o`.

`x` only terminates a currently discovered, identity-revalidated local process. It sends SIGTERM once and never escalates to SIGKILL. An inactive configured route has no process to terminate; use `d` to disable the route.

The bottom bar shows `ok`, `off`, `TCP-only`, or `URL-only` for `o`, `O`, and `y`. `NO_COLOR=1` keeps written state indicators while disabling color.

### Refresh and operation state

Configuration, listener discovery, and Tailscale readiness load asynchronously. The detail pane labels Serve and Funnel readiness, issues, remediation, and operation state independently. Long content wraps to the available pane width. Details remain organized into Summary, Alerts, Action Items, Listener, Exposure Routes, Readiness, Operation, and Safety.

Refresh is periodic with bounded backoff and never steals focus. After the first load, stale-while-revalidate keeps the last known list, details, selection, and focus visible while `↻ Refreshing...` is shown. Failed refreshes preserve cached rows and surface a warning. Completed, failed, cancelled, and unverified operations remain visible until a new observed state or explicit dismissal.

## Routes and safety

### Inactive and external routes

A configured route whose local listener is gone appears as `Inactive configured`. Tailge never automatically removes or re-enables it; review its scope and explicitly disable it when stale.

Routes not created and verified by the current Tailge session are external or unknown. They remain visible, but replacement and removal require explicit confirmation and exact provider identity.

### Transport and identity

Local listener discovery proves only that a TCP listener exists; it does not prove HTTP or HTTPS. Exposure mutations may therefore use exact raw-TCP selectors (`serve:tcp=...` or `funnel:tcp=...`) and never infer a protocol.

Route identity retains the provider selector, service, handler path, backend, target, mode, and observed URL. Tailscale's `AllowFunnel: true` is the evidence for public access; absent or false means private Serve. A permissions-only `AllowFunnel` payload with no handler is an authoritative empty route set, not an active exposure.

### Limitations

- Only the current machine is supported; remote-machine control is not implemented.
- TCP is supported; UDP is deferred.
- Tailscale behavior is version-specific. Unsupported or unverified exact operations remain read-only.
- Local status cannot prove tailnet reachability or public internet reachability; check those separately.
- Tailscale credentials and application secrets are outside Tailge.

## Exit codes and JSON

CLI commands return stable exit classes:

- `0` — success
- `2` — invalid, ambiguous, or unsupported input
- `3` — dependency unavailable or readiness is read-only/not ready
- `4` — permission denied
- `5` — timeout or cancellation
- `6` — operation or safety refusal
- `7` — verification or unknown state
- `8` — configuration failure
- `130` — interruption

JSON responses use `schema_version: 1` and include `data`, `warnings`, and/or structured `errors` where applicable.

## Development and CI

Run checks from the repository root:

```sh
make check
make quality FUZZTIME=1s
make cross-build
```

`make check` formats and tests the code, runs race tests, and invokes `go vet`. `make quality` adds linting, vulnerability scanning, and bounded parser fuzzing. GitHub Actions runs the same quality checks on pushes to `main`, pull requests, and manual dispatches. Its quality job installs pinned tools and builds Linux amd64 and Darwin arm64 artifacts; the commands are:

```sh
mkdir -p "$PWD/.tools/bin"
GOBIN="$PWD/.tools/bin" go install honnef.co/go/tools/cmd/staticcheck@2025.1.1
GOBIN="$PWD/.tools/bin" go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8
GOBIN="$PWD/.tools/bin" go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
PATH="$PWD/.tools/bin:$PATH" make quality FUZZTIME=1s
make cross-build
```

## Related documentation

- [`architecture.md`](architecture.md) — system context, internal control flow, and safety invariants.
- [`adr/0001-full-screen-tui.md`](adr/0001-full-screen-tui.md) — accepted full-screen workspace decision.
- [`../CONTEXT.md`](../CONTEXT.md) — domain language and interaction rules.
