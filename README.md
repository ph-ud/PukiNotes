# PukiNotes

A cozy, local-first whiteboard for two. No accounts, no server. Boards are shared peer-to-peer over tailcat,
and your own AI agent is the helper inside the app.

## How to start

Point your AI agent (any of them: Claude Code, Codex, Gemini CLI, Cursor…) at this repo and say:

> **launch PukiNotes**

Your agent reads [`AGENTS.md`](AGENTS.md), sets things up, launches the app, and binds itself to it. From then
on, ask it things in the ✦ AI panel, or write `@ai` in any comment on the board. That work runs in **your**
agent's session, on your plan. Your partner's `@ai` comments go to **their** agent.

To share a board, click **Share** and send your partner the code.

## What's here

- `AGENTS.md`: the launch and binding procedure, written for agents (`CLAUDE.md` just points to it)
- `cmd/pukinotes/`: the app. One Go program: local web server, tailcat peer sync, and the agent tools (MCP + CLI).
  The built UI is embedded, so Go is the only thing you need.
- `web/`: UI source (Yjs + plain JS). Only needed if you change the UI.
- `DESIGN.md`: design and architecture notes

## Starting it by hand

```
go run ./cmd/pukinotes start      # opens PukiNotes in your browser
go run ./cmd/pukinotes doctor     # checks everything
go run ./cmd/pukinotes stop
```

## Using PukiNotes

- **Drag empty space** to pan. **Scroll wheel** zooms. Trackpad scrolling pans, and pinching zooms.
- **P**: draw. Pause at the end of a stroke to snap it into a line, box or circle. Hold Shift for straight lines.
- **T**: table. Type numbers or formulas like `=SUM(C1:C4)`. Enter moves down and adds rows. Drag a column
  or row edge to resize it (double-click a column edge to fit), or drag the right edge to resize the whole table.
- **N**: sticky note, **C**: comment, **L**: arrow, **I** or ⌘V: image, **E**: eraser, **V**: select.
- **✦ AI**: your bound agent. Ask it things, then open **History** to revert anything it did.
- ⌘Z / ⇧⌘Z: undo / redo your own changes. Delete removes the selection.
- **Share**: create an invite code for the board you're on, or paste your partner's code to join theirs.

**Updating:** when there's something new on `main`, a dot appears on the PukiNotes menu (top left). Click
**Update now**: it pulls, rebuilds and restarts in a few seconds. Your boards aren't touched.

Boards are saved on your computer in `~/.pukinotes`. Shared boards sync whenever both PukiNotes apps are running.
