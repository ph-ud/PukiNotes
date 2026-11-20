package main

// pukinotes_add_image: the agent gives a web link or a local file, PukiNotes
// fetches it here and hands the window a data URL, which shrinks it the same
// way as a pasted photo.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// imageClient only talks to the public internet, so an image link can't be
// used to reach this computer or the home network.
var imageClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, _ := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				if os.Getenv("PUKINOTES_ALLOW_LOCAL_IMAGES") == "" {
					return errors.New("image links must be on the public internet")
				}
			}
			return nil
		}}).DialContext,
	},
}

var ogImage = regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)\s*=\s*["'](?:og:image(?::secure_url)?|twitter:image)["'][^>]*>`)
var metaContent = regexp.MustCompile(`(?is)content\s*=\s*["']([^"']+)["']`)

func fetch(ctx context.Context, src string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (PukiNotes)")
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.9,text/html;q=0.5")
	resp, err := imageClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("couldn't download the image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("couldn't download the image: the site answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	return data, resp.Header.Get("Content-Type"), err
}

// pageImage finds a web page's preview picture (og:image), so a product page link works too.
func pageImage(page string, html []byte) string {
	m := ogImage.Find(html)
	if m == nil {
		return ""
	}
	c := metaContent.FindSubmatch(m)
	if c == nil {
		return ""
	}
	base, err := url.Parse(page)
	ref, err2 := url.Parse(strings.ReplaceAll(string(c[1]), "&amp;", "&"))
	if err != nil || err2 != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

const maxImageBytes = 15 << 20

func loadImage(ctx context.Context, src string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", errors.New("give url (an https link to the image) or path (a local image file)")
	}
	var data []byte
	var ctype string
	if u, err := url.Parse(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		var err error
		if data, ctype, err = fetch(ctx, src); err != nil {
			return "", err
		}
		if !strings.HasPrefix(http.DetectContentType(data), "image/") && strings.Contains(strings.ToLower(ctype), "html") {
			img := pageImage(src, data)
			if img == "" {
				return "", errors.New("that link is a web page without a preview picture; link to the image file itself")
			}
			if data, ctype, err = fetch(ctx, img); err != nil {
				return "", err
			}
		}
	} else {
		p := strings.TrimPrefix(src, "file://")
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return "", fmt.Errorf("couldn't open the image file: %w", err)
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, maxImageBytes+1)); err != nil {
			return "", err
		}
	}
	if len(data) > maxImageBytes {
		return "", errors.New("that image is over 15 MB; pick a smaller one")
	}
	if ct := http.DetectContentType(data); strings.HasPrefix(ct, "image/") {
		ctype = ct // trust the bytes over the header
	} else if !strings.HasPrefix(ctype, "image/svg") && !strings.HasPrefix(ctype, "image/avif") {
		return "", fmt.Errorf("that isn't an image (it looks like %s). Link to the image file itself, not the page it's on", ct)
	}
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	return "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
