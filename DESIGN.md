# PukiNotes — design draft v0.1

A cozy, local-first infinite whiteboard for two people. No accounts, no PukiNotes server.
Your boards live on your computer; when you share one, the two computers talk directly
over an encrypted tunnel (tailcat), and every edit merges live.

> Status: first draft, 20 Nov 2026. The look-and-feel prototype (retired, kept locally in `tmp/`).
>

---

## 1. What it should feel like

**Cozy, warm, snappy.**

- **Warm paper, not a cold grid.** Cream background with soft dots, warm charcoal ink, terracotta accent,
  sage for your partner. Rounded panels, soft shadows, handwritten font on sticky notes.
- **Snappy means zero waiting.** Every action shows on screen in the same frame, before any disk or network
  work. Animations are short (120–220 ms) with a little spring, never blocking.
- **Together, but calm.** You see your partner's cursor and a small "Mo is here" pill. No notification spam.
- **Mouse-first.** Wheel zooms toward the cursor, drag on empty space pans, and single-letter shortcuts
  pick tools (V, H, P, L, N, C, I, E).

### Features in v1

| Feature | Notes |
|---|---|
| Infinite boards | Many boards per person, each with its own camera position |
| Sticky notes | 5 warm colors, double-click to write, show who wrote them |
| Paste images | ⌘V or drag-and-drop, shown as polaroids, resizable |
| Comments | Pin anywhere, threaded replies |
| Drawing with a "steady hand" | See below |
| Arrows | Shift snaps to 15° angles |
| Tables | A simple spreadsheet with formulas and totals (section 5) |
| Your AI agent | Any agent (Claude Code, Codex…) binds to PukiNotes, reads and edits the board, and every change can be reverted (section 6) |
| Undo / redo | Undoes **your own** changes only, never your partner's |

### Steady hand: drawing help for a mouse

1. **Stabilizer.** The line trails the cursor slightly (lazy-brush smoothing), which irons out wrist jitter.
2. **Curve fitting.** Finished strokes are simplified (Ramer–Douglas–Peucker) and drawn as smooth curves.
3. **Hold to snap.** Pause for about 0.4 s at the end of a stroke and it becomes a clean straight line,
   box or circle. While a snapped line is held, you can keep moving to adjust its end.
4. **Shift = straight line**, snapped to 15° steps.
5. Toggle it off any time for raw strokes.

Later: snap arrow ends to notes (connectors), and a better shape recognizer (triangles, stars, ticks).

---

## 2. Connecting: tailcat is the right pick

You asked about Tailscale vs tailcat. **Tailcat fits this app better.** It is Tailscale's open source
tool (released Aug 2026) that gives you Tailscale's encrypted WireGuard tunnel and NAT traversal with
**no account and no control server**.

| | Tailscale | tailcat |
|---|---|---|
| Accounts | Both people need one, and you share a device or a tailnet | None |
| Install | The Tailscale app on both computers | Nothing extra: it is a Go library we build into PukiNotes |
| Encryption | WireGuard | WireGuard plus a pre-shared key (default on) |
| Behind home routers | Yes | Yes (same hole-punching), falls back to a relay |
| Plug-and-play | No | Yes: paste a code |

### How tailcat works for us

- Each PukiNotes install has a **long-lived key pair**. PukiNotes embeds the tailcat Go library directly.
- A tailcat "address" (about 140 characters, starts with `tc`) contains your public key, a secret
  pre-shared key, and which relay to meet at. It works as a **bearer secret**, so it is only used once, for
  the invite.
- The computers first meet through a **DERP relay**, try to punch a direct UDP path, and switch to it when it
  works. A relay only forwards encrypted bytes. With the pre-shared key, a relay operator cannot join.

### Pairing flow (one time)

1. You click **Share** → PukiNotes creates a single-use invite (the tailcat address, shown as a short
   word code plus a copy button).
2. You send it over iMessage/WhatsApp. Your partner pastes it into PukiNotes.
3. Over that first encrypted tunnel, the two installs swap their **permanent public keys and addresses**,
   pin each other in an allow-list (tailcat drops handshakes from any other key), and then **retire the
   invite**. A leaked invite code is useless after this.
4. From then on, whichever computer opens PukiNotes first listens, and the other dials in automatically. No codes.

### Risks to plan for

- **Tailcat is new** and says it has no API or wire-format stability promise yet. We pin a version and wrap
  it behind a small `Transport` interface so we can swap it out.
- **Tailscale's public tailcat relays** are free but rate-limited, in a few regions, with no uptime promise.
  They are fine for two people sending small edits (and the connection usually goes direct anyway). For
  reliability, PukiNotes should support **your own relay** (a tiny `derper` on a $4 VPS), set in Settings.
- **Fallback transports** behind the same interface: plain Tailscale (via `tsnet`) for people who already use
  it, and LAN direct connect when you're on the same Wi-Fi.

---

## 3. The sync engine: making collaboration "just work"

The goal is that both people can edit the same board at the same time, go offline, keep editing, reconnect,
and **always end up with the same board, with nobody's work lost**. The proven way to do that is a **CRDT**
(conflict-free replicated data type): a data structure where edits from any order always merge to the same
result. The network then only has to deliver bytes; it never has to decide who wins.

### Recommended stack

- **Yjs** (CRDT) in the app's UI layer. It is mature and fast, has a built-in sync protocol, an
  "awareness" channel for cursors, and a per-user UndoManager.
  (Alternative: **Loro**, newer, with nice history/time-travel. Worth revisiting before M2.)
- **Go backend** (in the same app) does storage and runs the tailcat tunnel. It treats Yjs updates as
  opaque bytes, so the transport stays simple.

### Data model per board

```
Y.Doc (one per board)
├─ meta: Y.Map            name, created, schemaVersion
├─ items: Y.Map<id, Y.Map>   one entry per object
│    sticky  → { type, x, y, w, color, author, z, text: Y.Text }
│    stroke  → { type, shape, points | geometry, color, width, author, z }
│    image   → { type, x, y, w, blob: sha256, author, z }
│    comment → { type, x, y, thread: Y.Array<{by, text, at}> }
└─ (presence is NOT in the doc: it goes over the awareness channel)
```

Choices that avoid conflicts:

- **Maps keyed by id, not arrays.** Two people adding things never clash.
- **Stacking order is a fractional index string** (`z: "a0V"`), so "bring to front" by both people at once
  never reshuffles everything.
- **Sticky text is `Y.Text`.** Both of you can type in the same note and letters merge character by character.
- **Position fields are last-writer-wins.** If you both drag the same note, it lands where the last drag
  ended, which feels natural.
- **Strokes are immutable once finished.** While you draw, the in-progress points stream over the presence
  channel so your partner sees the line appear live, then it is committed as one object on mouse-up.
- **Images are stored outside the doc**, by content hash. The doc only holds the hash; the bytes travel
  separately over their own stream, with a blurry placeholder until they arrive.

### Wire protocol (over the tailcat tunnel)

One tailcat TCP connection, with small framed channels:

| Channel | Carries | Delivery |
|---|---|---|
| `hello` | app version, schema version, peer identity | reliable |
| `sync/<board>` | Yjs sync step 1/2 (state-vector diff), then live updates | reliable, ordered |
| `presence/<board>` | cursors, selections, live strokes, "typing in note X" | latest-wins, throttled to ~30 Hz |
| `blob` | image bytes by hash, on request | reliable, chunked, resumable |

- **On connect:** each side sends its state vector and receives exactly what it is missing. Offline edits
  from both sides merge in one round trip.
- **While connected:** each local edit is applied on screen first (optimistic, 0 ms), then batched every
  ~16–30 ms and sent.
- **Heartbeat every 2 s.** If the link drops, PukiNotes keeps working locally, shows "Mo went offline", and reconnects
  with backoff. Nothing is lost because the doc is the source of truth.
- Partner cursors are **interpolated** so they glide instead of jumping.

### Storage on disk

- `~/PukiNotes/boards/<id>.pukinotes`: one SQLite file per board with an append-only log of Yjs updates plus a
  periodic compacted snapshot, and image blobs. Writes go to a write-ahead log, so a crash never corrupts a board.
- Easy backups: copy the folder. Export to PNG/PDF later.

### How we make it flawless (testing)

1. **Convergence fuzz tests:** two or three simulated replicas apply thousands of random edits, delivered in
   random order with duplicates and drops, then we check that all boards end up identical.
2. **Network chaos tests:** run the real sync over a simulated link with latency, packet loss and
   disconnects, including long offline edits on both sides.
3. **Soak test:** two real PukiNotes instances across two networks over tailcat for hours, with a bot drawing.

### Gentle social cues, not locks

No hard locking. If your partner is typing in a note, you see a soft sage outline with "Mo is typing…".
You can still type there too and it merges.

---

## 4. Shipping: a repo any AI agent can launch

Since PukiNotes is just for the two of you, we skip installers and code signing. Everything is wrapped for AI agents,
and no specific vendor is required.

- **One private GitHub repo** with you both as collaborators. To start PukiNotes, you point your agent (Claude Code,
  Codex, Gemini CLI, Cursor…) at the repo and say **"launch PukiNotes"**.
- **`AGENTS.md` is the contract.** It's the cross-vendor convention for agent instructions. It covers setup,
  launch, binding, the request loop, the rules, and troubleshooting. `CLAUDE.md` is a one-line pointer to it, and
  pointer files for other agents can be added the same way.
- **One cross-platform entry point:** `go run ./cmd/pukinotes <setup|start|stop|status|tool|join|doctor>`. That needs
  no `make` and no shell scripts, so it works the same on macOS, Windows and Linux.
- **App shape:** one Go program that runs tailcat, serves the UI to a browser window on `127.0.0.1` (with a
  per-launch token), and exposes the agent tools over MCP and the CLI.
- **Board data never goes into git.** Boards live in `~/PukiNotes/` on each computer. The only thing you send her is
  the board invite code.
- **Version check on connect:** "Mo is on an older version. Ask them to pull."

## 5. Tables: a simple spreadsheet on the board

Tables are a board object like sticky notes, made for shared expenses, lists and basic accounting.

- Cells with **numbers, text and formulas**: `=SUM(C1:C4)`, `=C2*0.5`, `AVERAGE`, `MIN`, `MAX`, `COUNT`
  (in the prototype). Later: `IF`, and percentages.
- **Automatic totals row** under any number column.
- **Column types** (later): text, number, money, date, checkbox, and person (You/Mo), so "who paid" becomes a
  real field and splits can be calculated.
- Keyboard like a spreadsheet: Enter moves down and adds a row at the end, Tab moves right, Esc stops editing.
- Export to CSV.

**How tables sync:** rows and columns are lists of ids, and cells are stored as `rowId:colId → value`.
If you add a row while she edits a cell, both changes land. If you both edit the **same cell** at once, the
last edit wins, and History shows the other value so you can restore it. Formulas are stored using row/column
ids under the hood and shown as `A1`-style refs, so inserting a row never breaks a formula.

## 6. Your own agent, bound to the app

PukiNotes has **no built-in AI and no API keys**. Each of you binds your own agent to your own copy of PukiNotes, and AI
work runs in that agent's session, on that person's plan. Any agent that speaks MCP works, and agents without
MCP can use the CLI.

### Binding

1. Your agent launches PukiNotes (`go run ./cmd/pukinotes start`). PukiNotes prints its local MCP address and a short-lived
   **bind code**.
2. Your agent connects to PukiNotes's MCP server and calls `pukinotes_bind(code, agent, model)`.
3. PukiNotes shows **"Bound: Claude Code"** (or Codex, or whatever it is) next to the ✦ AI button. Only one agent is
   bound per computer, and binding again replaces it.

The bind code proves the agent is on this computer and was started by you. The MCP port only listens on
`127.0.0.1` and needs the session token.

### Talking to your agent

- **AI panel:** type a request, or pick a suggestion.
- **`@ai` in any comment:** "@ai add these receipts to the budget table". The agent's reply appears in that
  comment thread, so the conversation stays next to the thing it's about.

Under the hood, both become **requests** in a local queue. The agent sits in a simple loop: wait for the next
request (`pukinotes_next_request`, a long-poll), read the board, act with the write tools, then reply.

### Routing: whose agent answers?

**The agent on the computer of the person who asked.** Comments sync to both of you, but an `@ai` written by Mo
is only handed to Mo's agent. So:

- each of you pays for your own AI use, with no shared keys;
- her agent can't be steered by your comments, and yours can't be steered by hers;
- if the asker's agent isn't running, the comment shows "waiting for Mo's agent" and runs when it's back.

### What the agent can do

The same actions as the UI, never raw file access:

| Read | Write (every write carries the `request_id`) |
|---|---|
| `pukinotes_list_boards`, `pukinotes_read_board` (all objects as JSON, with table values computed), `pukinotes_board_snapshot` (PNG) | `pukinotes_add_sticky`, `pukinotes_update_item`, `pukinotes_move_items`, `pukinotes_delete_items`, `pukinotes_draw_shape`, `pukinotes_add_arrow`, `pukinotes_create_table`, `pukinotes_set_cells`, `pukinotes_add_rows`, `pukinotes_add_comment`, `pukinotes_revert_change` |

### Chats: several agents on one board (v0.3)

Both of you can have agents working on the same board at the same time. The ✦ panel holds **chats**.

- **A chat belongs to whoever started it** and is served by that person's agent, on that person's computer.
  Either of you can add messages to any chat; they queue for that chat's agent. If the owner's computer is away,
  the chat shows as paused and its messages run when it's back.
- **Chats live in the board** (a new `chats` map in the Yjs doc: owner, title, messages, per-message state, and
  the items its agent touched recently). Older copies ignore it. Only the owner's window hands messages to its agent,
  and the agent dedups by message id, so nothing runs twice.
- **No locks; a briefing instead.** Every request comes with a briefing on the other open chats: who started them,
  what they were asked, whether they're working, waiting or paused, and which items they touched recently. Agents
  steer around each other's items. A write that touches an item another chat just touched comes back with a
  `heads_up`.
- **The quiet safety net.** Every item has a version (a hash of its content). The window remembers which version
  its agent last read, and an agent edit to an item that changed since is refused as stale, so the agent re-reads
  and redoes it on the fresh item. Humans are never blocked; their edits merge as always.
- **One turn, one undo.** Everything an agent does for one message is one change set. Its reply in the chat shows
  an icon for each item it changed (hover to highlight, click to fly there) and one ↩ button that undoes the turn.
- **Icon-first panel.** One bubble per chat in the owner's color with a small ✦; a spinning ring means working, a
  number means messages waiting, grey with ⏸ means paused. ＋ starts a chat, the clock opens History. Items an
  agent just touched glow softly on the board. Agent status travels between the two computers as a small
  `agentst` message that older copies ignore.

### History and revert

- **Every change is kept,** whether you, Mo or an agent made it. History is synced, so you both see one timeline.
- **One request = one change set,** labeled with the request text, who asked, and which agent did it
  ("Claude Code for You", "Codex for Mo"). Each Yjs transaction is tagged with the change set's id.
- **Revert puts back only what the agent touched.** Edits either of you made since then stay. If someone changed the
  same object after the agent did, PukiNotes asks first.
- **Revert is a normal change,** so it syncs and can itself be undone. Yjs snapshots also allow "view the board as of 3 pm".
- **Guardrails:** writes without a `request_id` are rejected. Deleting more than about 10 objects or a whole table
  pauses for a human tap. Agents have no tools for sharing, keys or settings. Board text is treated as data,
  so a sticky note can't instruct an agent.

## 7. Milestones (updated)

v0.1 is tested end to end with two PukiNotes apps on one machine through a local relay: pairing, live sync,
cursors, restart and reconnect, an agent building a table, an `@ai` comment answered in its thread, and a
one-click revert that syncs to the partner. Known v0.1 limits: the PukiNotes window must be open for agent board
tools; a revert doesn't yet warn if someone edited the item after the agent; never tested on the public
tailcat relays from this sandbox (they're blocked here).


| | Goal | Done when |
|---|---|---|
| **M0** | Look-and-feel prototype | ✅ notes, drawing help, tables, Claude demo, History |
| **M1** | Real single-player app | ✅ v0.1: Go app with embedded web UI, Yjs boards saved to `~/.pukinotes`, all tools including tables, undo |
| **M2** | Sync engine | ✅ v0.1: Yjs updates stored as hashed blobs, have/want/upd sync, log compaction. Still to do: fuzz tests |
| **M3** | Tailcat pairing + live sync | ✅ v0.1: invite codes, pair secrets, presence, auto-reconnect, version check. Images ride inside the board (downscaled) |
| **M4** | Agent binding + History | ✅ v0.1: MCP + CLI tools, bind codes, request loop, `@ai` comments, change sets, selective revert, delete guardrails |
| **M5** | Polish | Table column types and CSV, connectors, evening theme |

## 8. Open questions for you

1. Which agent will she use? (Any works. It's just so we test that one first.)
2. Are Tailscale's free tailcat relays OK to start with, or do you want your own relay from day one?
3. Should shared boards always allow editing (my default), or do you also want view-only?
4. What currency should tables default to?

---

Sources: [tailcat README](https://raw.githubusercontent.com/tailscale/tailcat/main/README.md),
[Tailscale blog: tailcat](https://tailscale.com/blog/tailcat),
[Pinggy: Tailcat without a control plane](https://pinggy.io/blog/tailcat_tailscale_without_control_plane/)
