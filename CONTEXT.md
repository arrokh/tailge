# Tailge Context

Tailge helps a person inspect local services and deliberately manage their network exposure. It does not implicitly control service processes; the TUI may explicitly terminate one or more revalidated selected processes sequentially with SIGTERM after a guarded confirmation.

## Interaction language

**Service workspace**:
The primary full-screen place for viewing local listeners, their exposure state, and the details needed to decide what to do next. It uses a split view with a navigable item list and a live detail pane.
_Avoid_: REPL, command prompt

**Panel-first interaction**:
A keyboard-driven interaction style in which one panel has focus, selection is visible, and the detail panel follows the selected item without leaving the workspace. The service list is the default focus; panel changes are explicit.
_Avoid_: form-by-form prompt flow

**List focus**:
The default workspace focus where `j`/`k` and `Up`/`Down` move through service items and the detail pane updates to the selected item.
_Avoid_: cursor navigation through unrelated panels

**Incremental search**:
Workspace search updates the visible service items as each character is entered, while preserving the current selection whenever that item remains visible.
_Avoid_: submit-only filtering

**Adaptive split**:
The service workspace gives the list about 40% of a wide terminal and the detail pane the remaining space, while protecting a 53-column minimum list-pane width for its process-usage columns. On narrow terminals it collapses to one focused pane rather than squeezing text. `z` zooms the focused left or right pane to the full workspace content area; pressing `z` again restores the split.
_Avoid_: fixed equal panes, squeezed text

**Selection continuity**:
The workspace selects the first item initially, keeps the selected item by stable identity across refreshes, and chooses the nearest remaining item if it disappears.
_Avoid_: raw row-index selection

**Action preview**:
An exposure change is first shown as a modal describing the exact service, target, current route, requested action, and warnings; the change occurs only after explicit confirmation.
_Avoid_: immediate mutation, implicit confirmation

**Applying state**:
After confirmation, the selected target visibly enters Applying while the workspace remains usable; the completed, failed, cancelled, or unverified result remains attached to that target.
_Avoid_: hidden background mutation, transient-only result

**Detail focus**:
`Enter` moves attention from the selected list item to its details for inspection and never changes exposure state; `Esc` returns to the list.
_Avoid_: Enter-to-mutate

**Open route**:
`o` opens a provider-observed absolute HTTPS URL when one exists. If an active, exact Serve TCP route has no observed URL, an explicit `o` may resolve Tailscale status's reported `Self.DNSName` and combine it with the exact listener port to open a UI-only HTTP preview. `y` copies that same preview through terminal OSC 52; Funnel TCP without an observed URL remains TCP-only and cannot be opened or copied. For multiple observed HTTPS URLs, `o` and `y` ask the operator to select one exact route URL. `O` opens the local convenience URL `http://localhost:<port>/` for the selected listener. These previews do not change route identity or mutation behavior and do not assert that a listener speaks HTTP; never use them for safety decisions. `y` reaches the local terminal client over SSH/Mosh rather than only the host OS clipboard.
The bottom status distinguishes observed HTTPS availability, Serve TCP preview availability, and TCP-only Funnel routes; `O` reflects local listener/browser availability. The table's `SELECT` column shows `[ ]`, `[✓]`, or `[V]` for unselected, normal-selected, and active visual-range rows. Service List items sharing a port collapse into one display row; exposure actions use the available local port as the primary target. Color is supplementary.
_Avoid_: guessed URL for safety decisions, opening a route different from the exact observed target

**Process termination**:
`x` opens a focused confirmation for the selected listener process or marked process set; Enter confirms, every PID/listener/identity in the captured batch is revalidated, and only SIGTERM is sent. A refresh during confirmation keeps a valid batch open and cancels it if any captured process identity changes. An inactive configured exposure route has no current local process, so `x` reports that fact and directs the user to `d` to disable the route. While termination runs, the active listener row shows `TERMINATING` in the Status column; selected batches advance sequentially.
_Avoid_: implicit process control, PID-only retargeting, SIGKILL escalation

**Serve/Funnel route identity**:
Tailscale can report one underlying endpoint in both Serve and Funnel status JSON. `AllowFunnel: true` is the evidence that the endpoint is public; absent or false means the route is private Serve. The same hostname/URL is therefore not evidence of two active routes or two access scopes. Exact identity also retains the provider listener selector, service, handler path, backend, target, mode, and observed URL so replacement, removal, ownership, and rollback cannot silently change the route. Multiple handlers can share one provider selector; the handler path and complete route identity, not just that selector, distinguish the route to disable. IPv6 numeric backends remain exact unless the operator explicitly selects `--localhost-backend` for a named path or explicit HTTPS root; that opt-in uses hostname resolution and weakens the exact-address guarantee.
A provider status containing only `AllowFunnel` permissions and no handler is an authoritative empty route set: Funnel permission may remain enabled after its handler is removed. It is not an active exposure and must not force the workspace into `UNKNOWN`.
_Avoid_: treating identical status payloads as duplicate active Serve and Funnel routes

**Named HTTP path route**:
An explicit exposure route that uses Tailscale's HTTPS reverse proxy on standard port 443 to forward one named, single-segment path to the selected local HTTP listener. The path prefix is stripped before forwarding, so the application remains unchanged and handles requests as if mounted at `/`. Tailge does not infer HTTP from TCP discovery, does not expose the backend port in the browser URL, and does not create a root/default service. A manual lowercase slug or generated normalized process-name-plus-port slug is persisted in provider route state. Specific numeric IPv6 backends remain the default; wildcard listeners use the corresponding loopback address because wildcard addresses cannot be dialed. The CLI's `--localhost-backend` explicitly opts into `http://localhost:<port>` when Tailscale rejects a numeric IPv6 proxy destination, with a weaker address-family guarantee. Named paths remain a CLI route kind; the TUI's `b` shortcut is reserved for explicit same-port HTTPS roots. `o` and `y` are UI-only and never mutate exposure. Observed non-HTTPS provider URLs are refused; a Serve TCP MagicDNS preview is allowed only from Tailscale's reported DNS name and the exact active listener port, and does not affect route identity or imply HTTP.
_Avoid_: implicit HTTP detection, an implicit root handler, silent hostname substitution, or treating provider configuration as application health

**Explicit HTTPS root handler**:
An operator-selected `/` handler on an explicit HTTPS port, configured by the CLI with `exposure http serve TARGET --root --https-port PORT` or by pressing `b` for the selected TCP listener's exact local port. The browser endpoint is `https://<reported-MagicDNS>:P/`, and Tailscale proxies plain HTTP to the exact local backend at `127.0.0.1:P` (or the selected specific IPv6 address); the application and listener remain unchanged. It is private Serve-only; custom-port Funnel roots are not supported. Listener discovery still proves TCP only, so `b` is explicit HTTP intent. A conflicting exact `serve:tcp=P` route is replaced only after fresh identity checks and explicit confirmation; failure triggers exact rollback, and unexpected partial state is left for inspection rather than broad-reset. Numeric IPv6 backend identity is preserved by default. `--localhost-backend` explicitly opts into hostname resolution when numeric IPv6 proxying fails, accepting weaker address-family guarantees.
_Avoid_: assuming a listener speaks HTTP, inferring the HTTPS port from a local listener, or silently substituting `localhost`

**Shared HTTPS access scope**:
All HTTP handlers on one Tailscale hostname and HTTPS port, including an explicit root handler, share the endpoint's Serve/Funnel visibility. Serve remains private to authenticated tailnet access; Funnel makes every handler on that shared endpoint public. Tailge rejects a mixed private/public handler request and requires explicit Funnel confirmation that calls out the sibling-handler consequence.
_Avoid_: per-path privacy claims on one shared endpoint

**Compatibility probe**:
A doctor operation that uses one disposable loopback listener to verify the installed provider's exact mutation and cleanup behavior. It must validate authoritative local and provider state, acquire the shared mutation lock, clean up the exact observed route even after uncertain command completion, verify removal, and persist version evidence only after cleanup succeeds.
_Avoid_: broad reset, stale cleanup context, or readiness evidence from an unverified temporary route

**Service sections**:
The single navigable list groups items visually into local listeners, inactive configured routes, and unknown or unavailable items without creating separate navigation contexts.
_Avoid_: hidden route tabs, separate list modes

**Workspace chrome**:
Persistent top and bottom status areas keep identity, source freshness, Serve/Funnel and HTTPS-handler readiness, focus, search, operation state, warnings, and available keys visible while the user navigates. The bottom bar also displays the running build's short SHA for checked-out `HEAD` and a link to the Tailge repository. Grouped navigation hints adapt to terminal width; the selected sort mode is shown where space allows, while `o`/`y` and local `O` availability remain visible with compact labels when needed. At constrained widths the link label compacts to `GitHub` and the status line prioritizes active progress or focus/root-action status. The `b` shortcut indicates whether an explicit same-port private HTTPS root can be configured. For IPv6 listeners, `Ctrl+B` in the root confirmation opts into the localhost backend alias and the preview calls out the weakened address guarantee; `o` stays an open-only shortcut.
_Avoid_: hidden readiness, stderr-only warnings

**Sticky feedback**:
Critical safety warnings, unknown/stale states, and failed or unverified results remain visible until refreshed or explicitly dismissed, while their detail remains attached to the affected item.
_Avoid_: auto-hidden safety failure, toast-only error

**Panel focus aliases**:
The workspace supports Tab/Shift-Tab, h/l, and Left/Right as equivalent ways to move between list and detail focus, while keeping the list as the default.
_Avoid_: one-keyboard-style-only navigation

**Action selector**:
Exposure choices are selected in a centered modal over the workspace, then confirmed in a distinct step before any route changes. It initially focuses the current observed mode, or Disabled when no route exists or state is unsafe. While the selector is open, `d`, `s`, or `f` immediately chooses Disable, Serve, or Funnel and advances to the same Cancel-focused confirmation as arrows plus Enter; a blocked choice remains blocked, and no shortcut skips confirmation.
_Avoid_: inline mutation, hidden action choice

**Explicit confirmation**:
All exposure changes use focused Confirm/Cancel controls with Cancel selected by default. Funnel still shows a prominent public-internet warning; the user must explicitly move focus to Confirm before pressing Enter.
_Avoid_: default-confirm, accidental Enter

**Exact route choice**:
When one listener has multiple complete, distinct, active route identities, its aggregate state remains active and its mode is shown as multiple; multiplicity alone is not ambiguity. A broad mode change stays unavailable, while Disable opens a route picker and removes only the selected exact provider route. Routes sharing one HTTPS endpoint selector are distinguished by path and complete identity. Incomplete or duplicated route identity, or one route matching multiple local listeners, remains ambiguous and fails closed. Noninteractive raw-TCP callers use `--mode serve|funnel`; named HTTP path callers select an exact `--path`, while explicit root-route callers select `--root --https-port PORT`. The TUI `b` action is single-listener only and always chooses that listener's port for the private HTTPS root.
_Avoid_: marking known distinct routes ambiguous, silently disabling every route, target-only route choice

**Deterministic replacement rollback**:
A replacement may proceed only when the observed provider selector can be carried through an exact rollback if the new route fails; service-style selectors remain unchanged rather than being guessed.
_Avoid_: removing a route that cannot be restored exactly

**Background refresh**:
Periodic or manual refresh updates workspace data without stealing focus, opening dialogs, or losing stable selection; changes to the selected item are surfaced visibly.
_Avoid_: disruptive refresh, focus theft

**Guarded quit**:
Quitting is immediate while idle, but an Applying operation requires an explicit choice to stay or cancel-and-quit, with uncertainty reported if cancellation cannot verify provider state.
_Avoid_: silent abandonment, exit-means-success

**Workspace loading**:
The full-screen service workspace appears immediately and fills its panels as listener and readiness results arrive independently; unavailable data remains visibly unavailable.
_Avoid_: blocking pre-screen startup, outside-the-workspace loading

**Terminal lifecycle**:
After TTY validation, Bubble Tea owns alternate-screen and raw keyboard setup and restores terminal state on every normal, error, interrupt, resize/shutdown, and cancellation exit.
_Avoid_: raw-mode leaks, alternate-screen residue, non-TTY input blocking

**Detail scrolling**:
When detail focus is visible, j/k, Up/Down, and page controls scroll the selected item's details without changing selection; those keys navigate items when list focus is visible.
_Avoid_: hidden focus, selection changes while reading

**Active search**:
An accepted search query remains visible and filters the workspace until deliberately cleared; editing can be cancelled without losing the prior query.
_Avoid_: disappearing filter, irreversible search edit

**Compact service row**:
A fixed-height list row carries selection state, concise service/target identity, exposure state, and best-effort process CPU and memory when available. macOS uses physical footprint with RSS fallback; Linux uses RSS. Details and scan JSON include the best-effort process working directory when available. Unavailable measurements are marked with a dash, the source is identified in details/JSON, and full diagnostics belong to details.
_Avoid_: multiline list row, hidden diagnostics

**Redundant state indicator**:
A state is communicated with a compact written badge plus an icon and optional color, so the distinction remains available with NO_COLOR or limited visual perception.
_Avoid_: color-only state

**Disabled action**:
An unavailable or unsafe exposure choice remains visible in the action selector but cannot execute and explains its blocking reason.
_Avoid_: hidden capability, enabled unsafe action

**Command palette**:
The `:` palette provides searchable access to workspace commands while delegating exposure changes to the same action preview and confirmation flow.
_Avoid_: hidden shortcut, safety-bypassing command

**Optional mouse interaction**:
Mouse clicks may select items, change panel focus, or activate visible controls, but every workflow remains complete and safe from the keyboard.
_Avoid_: mouse-required action

**Scoped keymap**:
The active keyboard commands follow the focused workspace mode: normal shortcuts act only in the workspace, text input owns typed characters, and modals own confirmation/navigation keys.
_Avoid_: global mutation shortcuts, typing-triggered action

**Single-target action**:
`v` toggles only the current item. `V` enters/exits Vim-style visual-line selection; navigation extends the selection from its anchor without clearing previously selected rows. Exposure actions and `x` apply to the selected set sequentially, with independent exact-target or process-identity validation, per-target verification, and failure reporting. A multi-route target still requires one exact route selection and cannot be included in a batch disable.
_Avoid_: bulk exposure, broad disable

**Name-first order**:
Within each visual service section, items default to case-insensitive ascending service name, then address, port, and stable identity. `e` or `S` cycles Name ascending, Name descending, and unsorted observation order; existing explicit port/address/exposure preferences remain configurable. Sorting never changes target identity.
_Avoid_: unstable refresh order

**Visible system listener**:
System or privileged listeners remain in the list by default and may be hidden only as a presentation preference; visibility never grants or removes exposure authorization.
_Avoid_: silently hidden listener, visibility-as-permission

**Help modal**:
A scrollable full-screen explanation of workspace keys, aliases, states, readiness, and safety that overlays unchanged selection.
_Avoid_: undocumented shortcut, help-as-navigation

**Protected Applying refresh**:
Refresh may observe provider state during an Applying operation, but it cannot erase the operation lifecycle, start another mutation, or re-enable actions for that target before termination.
_Avoid_: refresh-cleared operation, concurrent mutation

**Explicit operation cancellation**:
Cancelling an Applying operation is a distinct confirmed action, followed by a fresh state read; if the final provider state is unknown, the result is Unverified.
_Avoid_: Esc-cancelled operation, exit-as-cancel

**Verified same-state no-op**:
Choosing the already verified current exposure mode performs no provider mutation and explains that the target is already in that state; unknown or stale state cannot qualify.
_Avoid_: unnecessary reapply, stale no-op

**Invalidated preview**:
A confirmation preview expires when its target or route fingerprint changes; the workspace closes it, preserves fresh observation, and requires a new selection.
_Avoid_: stale confirmation, port-only retarget

**Focused public confirmation**:
Funnel confirmation shows a prominent public-internet warning and requires explicitly moving focus to Confirm before pressing Enter; Cancel is the default.
_Avoid_: default-confirm, accidental Enter

**Fresh retry**:
Retrying a failed or unverified operation is a new action that refreshes state, presents a new preview, and requires confirmation rather than replaying stale intent.
_Avoid_: automatic retry, stale replay

**Detail sections**:
Details are organized as Summary, Alerts, Action Items, Listener, Exposure Routes, Readiness, Operation, and Safety. Serve and Funnel readiness, issues, remediation, and operation state are labeled independently; each action item identifies its owner and next step. State, readiness, route ownership, and route counts use compact text badges alongside written values; current target and observed URLs remain prominent.
_Avoid_: unstructured diagnostic dump

**Visible pane focus**:
The focused pane is identified by its colored border and persistent status bar; panel titles stay clean, while the selected service row retains a written marker and highlight. `z` zooms whichever pane has focus without changing selection or panel focus; focus navigation can choose the other pane while zoomed.
_Avoid_: ambiguous focus

**No-match detail state**:
When active search hides every item, details clear and show search-clearing guidance; no hidden item remains actionable or displayed as current.
_Avoid_: stale hidden details

**Automatic pane sizing**:
The workspace chooses pane geometry from terminal dimensions and does not expose manual split-resize controls. Split view requires at least 100 columns and 24 rows; below either threshold the workspace collapses to one pane. In split view, `z` toggles the focused list or details pane between the split and full-width/full-height content area.
_Avoid_: squeezed fixed layout, premature resize key

**Safe Vim sequence**:
A multi-key navigation sequence such as `gg` has a bounded pending state, can be cancelled with Esc, and cannot invoke an exposure action.
_Avoid_: ambiguous sequence, sequence-triggered mutation

**Standard search navigation**:
Search does not repurpose Vim `n`/`N`; filtered results use standard Up/Down keyboard navigation while the incremental query remains active.
_Avoid_: context-sensitive n/N
