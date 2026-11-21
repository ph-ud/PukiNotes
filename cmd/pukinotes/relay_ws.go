//go:build ts_debug_websockets

package main

// Built with -tags ts_debug_websockets: tailcat can reach its relays over
// WebSocket, which is what most cloud sandboxes' web proxies let through.
const wsRelays = true
