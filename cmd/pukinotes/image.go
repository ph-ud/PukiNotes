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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxImageBytes = 15 << 20

func loadImage(ctx context.Context, src string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", errors.New("give url (an https link to the image) or path (a local image file)")
	}
	var data []byte
	var ctype string
	if u, err := url.Parse(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", src, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (PukiNotes)")
		req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.8")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("couldn't download the image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("couldn't download the image: the site answered %s", resp.Status)
		}
		if data, err = io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1)); err != nil {
			return "", err
		}
		ctype = resp.Header.Get("Content-Type")
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
