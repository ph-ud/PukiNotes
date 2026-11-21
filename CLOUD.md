# PukiNotes in a cloud sandbox: instructions for agents

Read this when you run in a **cloud environment** instead of on your human's computer: Claude Code on the web or in
the Claude app, Codex cloud, or any container with no screen. Your human talks to you from a chat (often on their
phone). You run a copy of PukiNotes in your sandbox, pair it with your human's PukiNotes, work on the board for
them, and **show them the board in the chat**.

How to tell: no `DISPLAY`, you are root in a container, or outbound traffic goes through `HTTPS_PROXY`. On a normal
computer, follow [AGENTS.md](AGENTS.md) instead.

What this copy is: a second PukiNotes of the same human (it shows up as "Cloud"). It syncs with their computer over
tailcat, peer to peer, through a relay that can't read anything. Their computer must be running PukiNotes for the
boards to sync. Nothing here needs an account or a server.

---

## 1. Network (once per environment)

The sandbox must be allowed to reach tailcat's relays. Ask your human to allow these hosts in the environment's
network settings (in Claude Code on the web: the environment's **Network access → Allowed domains**). If you have a
tool that edits your environment's allowed hosts, propose the change yourself.

```
tailcat.dev  tc301a.ipn.dev  tc302a.ipn.dev  tc303a.ipn.dev  tc304a.ipn.dev
```

The relay list lives at `https://tailcat.dev/derpmap.json`. If a host there isn't on the list above, allow it too.

Sandboxes usually only let traffic out through a web proxy, which passes WebSocket but not tailcat's normal relay
protocol. So **always build with the WebSocket relay tag** (PukiNotes switches to it by itself when `HTTPS_PROXY` is
set):

```
export GOFLAGS=-tags=ts_debug_websockets
```

Put that in front of every `go run`, or set it once in your shell. Without it, `doctor` says
"this network only lets PukiNotes out through a web proxy".

## 2. Start PukiNotes

```
export GOFLAGS=-tags=ts_debug_websockets
go run ./cmd/pukinotes start        # run it in the background; it keeps running
```

- Go is the only requirement (`go version`). The first build downloads modules and takes a minute or two.
- With no screen, PukiNotes keeps its window open **headless** by itself (it uses Chrome or Chromium; set
  `PUKINOTES_BROWSER=/path/to/chromium` if it can't find one). The board tools need that window, so don't kill it.
- `go run ./cmd/pukinotes doctor` must show the relay as up and a window as open.
- Bind yourself exactly as in [AGENTS.md §3](AGENTS.md#3-bind-yourself-to-pukinotes) (MCP or the `tool` command line).

## 3. Connect to your human's boards

**If `PUKINOTES_STATE` is set** in your environment, PukiNotes restored its identity and pairings from it on start.
Wait about 20 seconds; `doctor` should show the partner as connected and `status` should list their boards. Done.

**Otherwise this is the first time.** Pair once:

1. Ask your human for their board's invite code: in PukiNotes on their computer, the orange **Share** button, top
   right. It starts with `pukinotes1-`.
2. `go run ./cmd/pukinotes join <code>`
3. Save the pairing so the next session reconnects by itself:
   ```
   go run ./cmd/pukinotes state export > pukinotes-state.txt
   ```
   That one line holds this copy's keys and pairings. **Treat it like a password**: don't paste it in the chat or
   commit it. Put the file where your human can copy it (for example a folder the chat shows them), and ask them to
   add it to the environment as a **secret environment variable** named `PUKINOTES_STATE`. Every later session then
   reconnects with no invite.

Only one cloud copy can use a saved state at a time: two sandboxes with the same state fight over the same address.

## 4. Do what your human asks in the chat

Your human asks in the chat, not in PukiNotes's AI panel. File each ask as a request, then work exactly as in
AGENTS.md (read the board, write with `request_id`, reply):

```
go run ./cmd/pukinotes ask "plan the living room: sofa, rug, TV wall"     # add --board ID for a specific board
# → {"request": {"id": "r…", "board": "…"}, …}
```

Use that `request.id` on every write, then `pukinotes_reply` with one sentence. Everything you do shows up live on
your human's computer and can be reverted there in one click. Requests from the AI panel on their computer still go
to the agent on that computer, not to you.

## 5. Show the board in the chat

Your human can't open `127.0.0.1` from their phone. Take a picture of the board instead:

```
go run ./cmd/pukinotes screenshot --out board.png             # first board; --board ID for another
go run ./cmd/pukinotes screenshot --size 390x844 --out board.png   # phone-shaped
```

It fits the view to everything on the board and renders the real app at 2× resolution. Then show it in the chat the
way your product shows pictures or pages. In Claude, publish an **Artifact**: a small HTML page with the PNG inlined
as a `data:image/png;base64,…` image, with a title like the board's name, which opens right inside the chat on the
phone. Update the same Artifact after each change, so the human keeps one link. If you can attach files to a reply
instead, attach the PNG.

## 6. Troubleshooting

| What you see | What to do |
|---|---|
| `doctor`: relay down, "Forbidden" / "no rule … allows this host" | §1: the hosts aren't allowed yet. Allowing takes up to 30 s to apply. |
| `doctor`: "only lets PukiNotes out through a web proxy" | Stop, then start again with `GOFLAGS=-tags=ts_debug_websockets`. |
| `doctor`: partner not connected | Their PukiNotes must be running. It reconnects within a minute. |
| `join`: "not paired" / "invite code isn't valid anymore" | Ask for the current code (Share). |
| Board tools time out | The headless window died; PukiNotes restarts it within ~20 s. Check `doctor`. |
| `PUKINOTES_STATE: … damaged` | The variable was cut while copying. Export it again from a working session. |
| "port 47321 is busy" | A copy is already running: `go run ./cmd/pukinotes status`. |
