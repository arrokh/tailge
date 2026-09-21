# Tailge project instructions

## Exposure transport and browser shortcuts

- Local listener discovery proves a TCP listener, not an HTTP protocol. Exposure mutations may therefore use an exact raw-TCP selector (`serve:tcp=...` or `funnel:tcp=...`) and must not silently infer HTTP/HTTPS.
- `o` opens an explicitly observed HTTP/HTTPS exposure URL when one exists. For an observed Serve TCP route without a URL, an explicit user press of `o` may resolve Tailscale's reported DNS name plus the exact listener port and use an HTTP browser preview by default; this is UI-only and must not affect route identity or mutation. Funnel TCP routes without an observed URL remain `TCP-only`. Never use a guessed URL for safety decisions.
- `O` is a local HTTP convenience URL (`http://localhost:<port>/`). It is not proof that a remote raw-TCP exposure speaks HTTP.
- `y` copies an explicitly observed browser URL; for a Serve TCP route without one, it resolves and copies the same provider-DNS HTTP preview used by `o`. Funnel TCP without an observed URL remains not copyable.
- `x` terminates only a currently discovered, identity-revalidated local process. An inactive configured route has no process to terminate; explain that and direct the user to `d` for route removal.
- Keep shortcut availability visible in the workspace status bar. Preserve fail-closed behavior for missing, stale, ambiguous, or incomplete route identity.

## Running the current binary

- `make cross-build` writes only `dist/` artifacts. When manually testing the local `./tailge` binary after a code change, run `make build` (or use `make tui`/`make run`) first so an older binary is not mistaken for the current implementation.

## Documentation and tests

- Keep the shortcut and transport semantics synchronized across `CONTEXT.md`, `README.md`, and `docs/architecture.md`.
- Add focused tests for route transport classification, shortcut status, and user-facing TCP-only feedback when changing this behavior.
- Preserve the full-screen Bubble Tea workspace and the exact route identity/safety decisions documented in `CONTEXT.md` and `docs/adr/0001-full-screen-tui.md`.
