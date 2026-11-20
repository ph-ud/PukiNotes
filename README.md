# Casa

A cozy, local-first whiteboard for two. No accounts, no server. Boards are shared peer-to-peer over tailcat,
and your own AI agent is the helper inside the app.

## How to start

Point your AI agent (any of them: Claude Code, Codex, Gemini CLI, Cursor…) at this repo and say:

> **launch Casa**

Your agent reads [`AGENTS.md`](AGENTS.md), sets things up, launches the app, and binds itself to it. From then
on, ask it things in the ✦ AI panel, or write `@ai` in any comment on the board. That work runs in **your**
agent's session, on your plan. Your partner's `@ai` comments go to **their** agent.

To share a board, click **Share** and send your partner the code.

## What's here

- `AGENTS.md`: the launch and binding procedure, written for agents (`CLAUDE.md` just points to it)
- `cmd/casa/`: the app. One Go program: local web server, tailcat peer sync, and the agent tools (MCP + CLI).
  The built UI is embedded, so Go is the only thing you need.
- `web/`: UI source (Yjs + plain JS). Only needed if you change the UI.
- `DESIGN.md`: design and architecture notes
- `prototype/index.html`: the original look-and-feel prototype

## Starting it by hand

```
go run ./cmd/casa start      # opens Casa in your browser
go run ./cmd/casa doctor     # checks everything
go run ./cmd/casa stop
```

## Using Casa

- **Drag empty space** to pan. **Scroll wheel** zooms. Trackpad scrolling pans, and pinching zooms.
- **P**: draw. Pause at the end of a stroke to snap it into a line, box or circle. Hold Shift for straight lines.
- **T**: table. Type numbers or formulas like `=SUM(C1:C4)`. Enter moves down and adds rows.
- **N**: sticky note, **C**: comment, **L**: arrow, **I** or ⌘V: image, **E**: eraser, **V**: select.
- **✦ AI**: your bound agent. Ask it things, then open **History** to revert anything it did.
- ⌘Z / ⇧⌘Z: undo / redo your own changes. Delete removes the selection.
- **Share**: create an invite code for the board you're on, or paste your partner's code to join theirs.

Boards are saved on your computer in `~/.casa`. Shared boards sync whenever both Casas are running.
