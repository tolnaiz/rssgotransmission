package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mmcdole/gofeed"
)

// Config holds runtime configuration.
type Config struct {
	Feeds           []string      // RSS/Atom feed URLs
	PollInterval    time.Duration // how often to poll
	StatePath       string        // where we persist seen items
	TransmissionURL string        // e.g. http://transmission:9091/transmission/rpc
	Username        string
	Password        string
	DownloadDir     string // optional, empty => Transmission default
}

// State keeps a set of seen item IDs so we don't re-add them.
type State struct {
	Seen map[string]bool `json:"seen"`
	mu   sync.Mutex       `json:"-"`
}

func loadState(path string) (*State, error) {
	st := &State{Seen: map[string]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *State) save(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// atomic-ish write
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) markSeen(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Seen[id] = true
}

func (s *State) isSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seen[id]
}

// Transmission client with session-id handling

type Transmission struct {
	URL      string
	User     string
	Pass     string
	Client   *http.Client
	sessID   string
	sessLock sync.Mutex
}

type rpcReq struct {
	Method    string      `json:"method"`
	Arguments interface{} `json:"arguments,omitempty"`
}

type rpcResp struct {
	Result   string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

func NewTransmission(u, user, pass string) *Transmission {
	return &Transmission{
		URL:    u,
		User:   user,
		Pass:   pass,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (t *Transmission) do(ctx context.Context, payload any) (*rpcResp, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, strings.NewReader(string(b)))
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	if t.User != "" {
		req.SetBasicAuth(t.User, t.Pass)
	}
	// ensure session id
	resp, err := t.doWithSession(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("transmission bad status: %s: %s", resp.Status, string(body))
	}
	var r rpcResp
	if err := json.Unmarshal(body, &r); err != nil { return nil, err }
	if strings.ToLower(r.Result) != "success" {
		return nil, fmt.Errorf("transmission result: %s", r.Result)
	}
	return &r, nil
}

func (t *Transmission) doWithSession(req *http.Request) (*http.Response, error) {
	t.sessLock.Lock()
	sess := t.sessID
	t.sessLock.Unlock()
	if sess != "" {
		req.Header.Set("X-Transmission-Session-Id", sess)
	}
	resp, err := t.Client.Do(req)
	if err != nil { return nil, err }
	if resp.StatusCode == 409 { // need/refresh session id
		newSess := resp.Header.Get("X-Transmission-Session-Id")
		_ = resp.Body.Close()
		if newSess == "" {
			return nil, fmt.Errorf("missing session id after 409")
		}
		t.sessLock.Lock()
		t.sessID = newSess
		t.sessLock.Unlock()
		// retry
		req2 := req.Clone(req.Context())
		req2.Header.Set("X-Transmission-Session-Id", newSess)
		return t.Client.Do(req2)
	}
	return resp, nil
}

// Add magnet or torrent URL to Transmission
func (t *Transmission) Add(ctx context.Context, uri string, downloadDir string) error {
	args := map[string]any{
		"filename": uri,
	}
	if downloadDir != "" {
		args["download-dir"] = downloadDir
	}
	_, err := t.do(ctx, rpcReq{Method: "torrent-add", Arguments: args})
	return err
}

// Utility to create a stable ID from feed item
func itemID(it *gofeed.Item) string {
	// Prefer GUID, then link, then title+date hash
	if it.GUID != "" { return it.GUID }
	if it.Link != "" { return it.Link }
	h := sha1.Sum([]byte(it.Title + it.Published + it.Updated))
	return hex.EncodeToString(h[:])
}

func pickTorrentURI(it *gofeed.Item) string {
	// Try enclosure URL first
	if it.Enclosures != nil {
		for _, e := range it.Enclosures {
			ct := strings.ToLower(e.Type)
			if strings.Contains(ct, "bittorrent") || strings.HasSuffix(strings.ToLower(e.URL), ".torrent") {
				return e.URL
			}
		}
	}
	// Fallback: link could be a magnet
	if strings.HasPrefix(it.Link, "magnet:") {
		return it.Link
	}
	// Some feeds place magnet in item content
	if it.Content != "" && strings.Contains(it.Content, "magnet:") {
		idx := strings.Index(it.Content, "magnet:")
		end := strings.IndexAny(it.Content[idx:], " \"'\n\t<")
		if end == -1 { end = len(it.Content) - idx }
		candidate := it.Content[idx : idx+end]
		return candidate
	}
	return ""
}

func parseEnvDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil { return d }
		log.Printf("WARN: invalid %s=%q, using %s", key, v, def)
	}
	return def
}

func mustURL(u string) string {
	if u == "" { return u }
	_, err := url.Parse(u)
	if err != nil {
		log.Fatalf("invalid URL %q: %v", u, err)
	}
	return u
}

func loadConfig() Config {
	feedsEnv := strings.TrimSpace(os.Getenv("FEEDS"))
	feeds := []string{}
	if feedsEnv != "" {
		for _, f := range strings.Split(feedsEnv, ",") {
			f = strings.TrimSpace(f)
			if f != "" { feeds = append(feeds, f) }
		}
	}
	interval := parseEnvDuration("INTERVAL", 15*time.Minute)
	statePath := os.Getenv("STATE_PATH")
	if statePath == "" { statePath = "/data/state.json" }
	transURL := mustURL(os.Getenv("TRANSMISSION_URL"))
	user := os.Getenv("TRANSMISSION_USER")
	pass := os.Getenv("TRANSMISSION_PASS")
	dldir := os.Getenv("DOWNLOAD_DIR")

	// Flags override env if provided
	flagFeeds := flag.String("feeds", strings.Join(feeds, ","), "Comma-separated feed URLs")
	flagInterval := flag.Duration("interval", interval, "Poll interval (e.g. 10m, 1h)")
	flagState := flag.String("state", statePath, "Path to state file")
	flagTrans := flag.String("transmission", transURL, "Transmission RPC URL")
	flagUser := flag.String("user", user, "Transmission username")
	flagPass := flag.String("pass", pass, "Transmission password")
	flagDL := flag.String("download-dir", dldir, "Optional Transmission download directory")
	flag.Parse()

	f := []string{}
	if *flagFeeds != "" {
		for _, x := range strings.Split(*flagFeeds, ",") {
			x = strings.TrimSpace(x)
			if x != "" { f = append(f, x) }
		}
	}

	return Config{
		Feeds:           f,
		PollInterval:    *flagInterval,
		StatePath:       *flagState,
		TransmissionURL: *flagTrans,
		Username:        *flagUser,
		Password:        *flagPass,
		DownloadDir:     *flagDL,
	}
}

func run(ctx context.Context, cfg Config) error {
	if len(cfg.Feeds) == 0 {
		return fmt.Errorf("no feeds configured")
	}
	if cfg.TransmissionURL == "" {
		return fmt.Errorf("TRANSMISSION_URL is required")
	}

	state, err := loadState(cfg.StatePath)
	if err != nil { return err }
	client := NewTransmission(cfg.TransmissionURL, cfg.Username, cfg.Password)
	parser := gofeed.NewParser()

	poll := func() {
		for _, f := range cfg.Feeds {
			log.Printf("Polling feed: %s", f)
			feed, err := parser.ParseURLWithContext(f, ctx)
			if err != nil {
				log.Printf("ERROR: parse %s: %v", f, err)
				continue
			}
			added := 0
			for _, it := range feed.Items {
				id := itemID(it)
				if id == "" { continue }
				if state.isSeen(id) { continue }
				uri := pickTorrentURI(it)
				if uri == "" {
					// Not directly a torrent/magnet; skip and mark seen to avoid repeated scans
					state.markSeen(id)
					continue
				}
				if err := client.Add(ctx, uri, cfg.DownloadDir); err != nil {
					log.Printf("ERROR: add to Transmission: %v (item: %s)", err, it.Title)
					continue
				}
				state.markSeen(id)
				added++
				log.Printf("Added: %q", it.Title)
			}
			if err := state.save(cfg.StatePath); err != nil {
				log.Printf("ERROR: saving state: %v", err)
			}
			log.Printf("Feed %s: added %d new item(s)", f, added)
		}
	}

	// initial run
	poll()
	if cfg.PollInterval <= 0 { return nil }
	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			poll()
		}
	}
}

func main() {
	cfg := loadConfig()
	log.Printf("Starting rssgotransmission; feeds=%d, interval=%s, state=%s, trans=%s", len(cfg.Feeds), cfg.PollInterval, cfg.StatePath, cfg.TransmissionURL)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}
