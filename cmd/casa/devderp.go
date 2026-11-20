package main

// A tiny local relay for testing two Casa copies on one computer without
// internet access. Not used normally.
//
//	CASA_DEV_DERP=serve:/tmp/region.json   run a local relay and write its details
//	CASA_DEV_DERP=use:/tmp/region.json     use a relay started by another copy

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func devDERPRegion() (*tailcfg.DERPRegion, error) {
	v := os.Getenv("CASA_DEV_DERP")
	if v == "" {
		return nil, nil
	}
	mode, path, _ := strings.Cut(v, ":")
	if mode == "use" {
		for i := 0; i < 50; i++ {
			if b, err := os.ReadFile(path); err == nil {
				var r tailcfg.DERPRegion
				if err := json.Unmarshal(b, &r); err == nil {
					return &r, nil
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil, os.ErrNotExist
	}
	d := derpserver.New(key.NewNode(), func(string, ...any) {})
	// Reuse the previous port when we can, so saved peer addresses stay valid.
	addr := "127.0.0.1:0"
	if b, err := os.ReadFile(path); err == nil {
		var old tailcfg.DERPRegion
		if json.Unmarshal(b, &old) == nil && len(old.Nodes) > 0 && old.Nodes[0].DERPPort > 0 {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(old.Nodes[0].DERPPort))
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
	}
	h := httptest.NewUnstartedServer(derpserver.Handler(d))
	h.Listener = ln
	h.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	h.StartTLS()
	r := &tailcfg.DERPRegion{RegionID: 900, RegionCode: "casa-dev", RegionName: "Casa dev relay", Nodes: []*tailcfg.DERPNode{{
		Name: "1", RegionID: 900, HostName: "localhost", IPv4: "127.0.0.1", IPv6: "none", STUNPort: -1,
		DERPPort: ln.Addr().(*net.TCPAddr).Port, InsecureForTests: true}}}
	b, _ := json.Marshal(r)
	return r, os.WriteFile(path, b, 0o600)
}
