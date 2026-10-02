# Tailge project instructions

## Exposure transport and browser shortcuts

- Local listener discovery proves a TCP listener, not an HTTP protocol. Preserve raw-TCP exposure for arbitrary services; only explicit HTTP-path/root operations may use Tailscale's HTTPS reverse proxy. The TUI `b` flow is explicit HTTP intent for a private same-port root. If replacing an exact conflicting `serve:tcp=P` route, require fresh identity checks and explicit confirmation, remove only that route, verify each step, and restore the captured exact route on failure—never use a broad reset.
- Preserve specific bind addresses when generating provider backends. Wildcard binds cannot be dialed, so map `0.0.0.0` to `127.0.0.1` and `::` to `::1`; do not otherwise silently substitute addresses.
- `o` opens an explicitly observed absolute HTTPS URL when available. For an exact active Serve TCP route without an observed URL, an explicit `o` may resolve Tailscale status's reported `Self.DNSName` plus the exact listener port into a UI-only HTTP preview; `y` may copy that same preview. This must not change route identity or mutation behavior, and must never be used for safety decisions. Funnel TCP without an observed URL remains `TCP-only`. `O` opens only the local convenience URL `http://localhost:<port>/`; it is not evidence that a remote raw-TCP route speaks HTTP. Use the Tailscale-reported DNS name, never the OS hostname, and preserve fail-closed behavior for missing, stale, overlapping, or incomplete identity.
- Preserve numeric IPv6 backends by default for named HTTPS paths and explicit HTTPS roots. `--localhost-backend` in the CLI or `Ctrl+B` in the TUI `b` HTTPS-root confirmation is an explicit opt-in to hostname resolution for IPv6 listeners; show that it weakens exact-address guarantees and never substitute it silently. Named HTTP paths remain CLI-only; the TUI `p` flow is removed.
- `x` terminates only a currently discovered, identity-revalidated local process. An inactive configured route has no process to terminate; explain that and direct the user to `d` for route removal.
- Keep shortcut availability visible in the workspace status bar. Multiple complete, distinct, active routes for one listener are a known `MULTI` state, not identity ambiguity; keep broad mode changes blocked and route Disable through an exact-route chooser. Preserve fail-closed behavior for missing, stale, overlapping, or incomplete route identity.

## Running the current binary

- `make cross-build` writes only `dist/` artifacts. When manually testing the local `./tailge` binary after a code change, run `make build` (or use `make tui`/`make run`) first so an older binary is not mistaken for the current implementation.

## Documentation and tests

- Keep the shortcut and transport semantics synchronized across `CONTEXT.md`, `README.md`, and `docs/architecture.md`.
- Add focused tests for route transport classification, shortcut status, and user-facing TCP-only feedback when changing this behavior.
- Preserve the full-screen Bubble Tea workspace and the exact route identity/safety decisions documented in `CONTEXT.md` and `docs/adr/0001-full-screen-tui.md`.

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues for `arrokh/tailge`; use the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Use the five default triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, and `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context repo: read root `CONTEXT.md` and relevant ADRs before work; use the glossary and flag conflicts. See `docs/agents/domain.md`.
