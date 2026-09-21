---
status: accepted
---

# Use a full-screen Bubble Tea workspace

Tailge will replace its line-oriented TUI prototype with a full-screen Bubble Tea application using Bubbles and Lipgloss. This supports the required split service workspace, panel focus, incremental search, responsive layout, and modal confirmations while preserving the existing safety and domain boundaries; continuing to extend the REPL would make those interactions brittle and difficult to verify.

The interactive program requires an interactive TTY, then lets Bubble Tea enter alternate-screen and raw keyboard modes. Bubble Tea owns terminal restoration on normal, error, interrupt, resize/shutdown, and cancellation paths. The workspace uses a roughly 40/60 split only at 100 or more columns and 24 or more rows; smaller terminals collapse to one pane and terminals too small for compact content show a resize notice. Non-interactive CLI commands remain unchanged and there is no REPL fallback.
