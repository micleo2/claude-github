// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Command tether manages on-demand files by path:
//
//	tether status [PATH]   list files and whether they are local, pinned or online-only
//	tether pin PATH        keep PATH (file or directory) local, downloading it now
//	tether unpin PATH      let PATH be evicted again
//	tether evict PATH      free the local copy of PATH now (it stays available online;
//	                       -verify re-reads it first)
//	tether hydrate PATH    download PATH now without pinning it
//	tether walkers [PATH]  tree walks (grep -r, builds, ...) being prefetched for,
//	                       and recent ones
//
// PATH may be inside a folder's on-demand view or its real path. The daemon
// is found through its config.xml (--home, $STHOMEDIR or the default
// locations), or --api/--apikey ($TETHER_API, $TETHER_APIKEY).
package main

import (
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

type client struct {
	base   string
	apikey string
	http   *http.Client
}

type folder struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	OnDemand     bool   `json:"onDemand"`
	OnDemandView string `json:"onDemandView"`
}

type walker struct {
	Exe        string    `json:"exe"`
	PGID       int       `json:"pgid"`
	Root       string    `json:"root"`
	Order      string    `json:"order"`
	Ended      time.Time `json:"ended"`
	Opened     int       `json:"opened"`
	Prefetched int       `json:"prefetched"`
	Bytes      int64     `json:"bytes"`
	Evicted    int       `json:"evicted"`
	Capped     bool      `json:"capped"`
	Excluded   []string  `json:"excluded"`
}

type fileState struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Pinned bool   `json:"pinned"`
}

func main() {
	home := flag.String("home", os.Getenv("STHOMEDIR"), "tether/syncthing home directory (contains config.xml)")
	api := flag.String("api", os.Getenv("TETHER_API"), "API base URL, e.g. http://127.0.0.1:8384")
	apikey := flag.String("apikey", os.Getenv("TETHER_APIKEY"), "API key")
	verify := flag.Bool("verify", false, "evict: re-read and check every block first (default: size and modification time, as the scanner)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tether [flags] status|pin|unpin|evict|hydrate|walkers [PATH]\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cmd, arg := flag.Arg(0), "."
	if flag.NArg() > 1 {
		arg = flag.Arg(1)
	}

	c, err := newClient(*home, *api, *apikey)
	if err == nil {
		err = run(c, cmd, arg, *verify)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tether:", err)
		os.Exit(1)
	}
}

func run(c *client, cmd, arg string, verify bool) error {
	folderID, rel, err := c.resolve(arg)
	if err != nil {
		return err
	}
	q := url.Values{"folder": {folderID}}
	switch cmd {
	case "status":
		q.Set("prefix", rel)
		var files []fileState
		if err := c.do(http.MethodGet, "/rest/ondemand/status?"+q.Encode(), &files); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		var local, online int64
		for _, f := range files {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", f.State, humanSize(f.Size), f.Name)
			if f.State == "online-only" {
				online += f.Size
			} else {
				local += f.Size
			}
		}
		tw.Flush()
		fmt.Printf("\n%d files, %s local, %s online-only\n", len(files), humanSize(local), humanSize(online))
		return nil
	case "walkers":
		var walkers []walker
		if err := c.do(http.MethodGet, "/rest/ondemand/walkers?"+q.Encode(), &walkers); err != nil {
			return err
		}
		if len(walkers) == 0 {
			fmt.Println("no tree walks seen")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "STATE\tPROGRAM\tPGID\tUNDER\tORDER\tOPENED\tPREFETCHED\tNOTES")
		for _, w := range walkers {
			state := "active"
			if !w.Ended.IsZero() {
				state = "ended " + time.Since(w.Ended).Round(time.Second).String() + " ago"
			}
			var notes []string
			if w.Capped {
				notes = append(notes, "reached crawlPrefetchMaxMiB")
			}
			if w.Evicted > 0 {
				notes = append(notes, fmt.Sprintf("%d evicted unopened", w.Evicted))
			}
			if len(w.Excluded) > 0 {
				notes = append(notes, "skips "+strings.Join(w.Excluded, ","))
			}
			root := w.Root
			if root == "" {
				root = "."
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d\t%d (%s)\t%s\n", state, filepath.Base(w.Exe), w.PGID, root,
				w.Order, w.Opened, w.Prefetched, humanSize(w.Bytes), strings.Join(notes, "; "))
		}
		return tw.Flush()
	case "pin", "unpin", "evict", "hydrate":
		q.Set("path", rel)
		if cmd == "evict" && verify {
			q.Set("verify", "true")
		}
		var res struct {
			Files int    `json:"files"`
			Error string `json:"error"`
		}
		err := c.do(http.MethodPost, "/rest/ondemand/"+cmd+"?"+q.Encode(), &res)
		if res.Error != "" {
			err = errors.New(res.Error)
		}
		if cmd == "evict" || cmd == "hydrate" {
			fmt.Printf("%s: %d files\n", cmd, res.Files)
		}
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// resolve maps a filesystem path to a folder ID and a folder-relative path.
func (c *client) resolve(arg string) (string, string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", "", err
	}
	var folders []folder
	if err := c.do(http.MethodGet, "/rest/config/folders", &folders); err != nil {
		return "", "", err
	}
	for _, f := range folders {
		if !f.OnDemand {
			continue
		}
		for _, root := range []string{f.OnDemandView, f.Path} {
			root = filepath.Clean(root)
			if root == "." || root == "" {
				continue
			}
			if abs == root {
				return f.ID, "", nil
			}
			if rel, ok := strings.CutPrefix(abs, root+string(filepath.Separator)); ok {
				return f.ID, filepath.ToSlash(rel), nil
			}
		}
	}
	return "", "", fmt.Errorf("%s is not inside an on-demand folder", abs)
}

func (c *client) do(method, path string, out any) error {
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apikey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil && resp.StatusCode < 300 {
		return fmt.Errorf("decoding response: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func newClient(home, api, apikey string) (*client, error) {
	c := &client{
		base:   strings.TrimRight(api, "/"),
		apikey: apikey,
		http: &http.Client{
			Timeout: 30 * time.Minute, // pin/hydrate return when the download is done
			// The GUI uses a self-signed certificate by default.
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
		},
	}
	if c.base != "" && c.apikey != "" {
		return c, nil
	}
	cfgPath, err := findConfig(home)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		GUI struct {
			TLS     bool   `xml:"tls,attr"`
			Address string `xml:"address"`
			APIKey  string `xml:"apikey"`
		} `xml:"gui"`
	}
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", cfgPath, err)
	}
	if c.base == "" {
		scheme := "http"
		if cfg.GUI.TLS {
			scheme = "https"
		}
		addr := cfg.GUI.Address
		if strings.HasPrefix(addr, "0.0.0.0:") {
			addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
		}
		c.base = scheme + "://" + addr
	}
	if c.apikey == "" {
		c.apikey = cfg.GUI.APIKey
	}
	return c, nil
}

func findConfig(home string) (string, error) {
	var cands []string
	if home != "" {
		cands = append(cands, filepath.Join(home, "config.xml"))
	} else {
		uh, _ := os.UserHomeDir()
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			cands = append(cands, filepath.Join(xdg, "syncthing", "config.xml"))
		}
		cands = append(cands,
			filepath.Join(uh, ".local", "state", "syncthing", "config.xml"),
			filepath.Join(uh, ".config", "syncthing", "config.xml"))
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config.xml found (tried %s); use --home or --api/--apikey", strings.Join(cands, ", "))
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
