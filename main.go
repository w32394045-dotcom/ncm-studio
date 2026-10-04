// Command ncm-studio decrypts NetEase Cloud Music .ncm files into tagged
// MP3/FLAC, fetching and embedding lyrics along the way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"ncm-studio/internal/backfill"
	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/pipeline"
	"ncm-studio/internal/store"
	"ncm-studio/internal/web"
)

func main() {
	var (
		host      = flag.String("host", "127.0.0.1", "address to bind; use 0.0.0.0 to serve the local network")
		port      = flag.Int("port", 8080, "port to listen on")
		dir       = flag.String("dir", "", "directory to scan for .ncm files (default: home)")
		output    = flag.String("output", "", "directory for decrypted files (default: <dir>/ncm-output)")
		workers   = flag.Int("workers", 3, "number of files to process at once")
		lyrics    = flag.String("lyrics", "both", "lyrics handling: off, embed, both (embed + .lrc), or file (.lrc only)")
		workspace = flag.String("workspace", "", "directory for the cover library and caches (default: beside the program)")
		configDir = flag.String("config", "", "directory for the settings file")
		version   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Println("ncm-studio", buildVersion)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	if *dir == "" {
		*dir = home
	}
	if *output == "" {
		*output = filepath.Join(*dir, "ncm-output")
	}
	if *configDir == "" {
		*configDir = defaultConfigDir(home)
	}

	mode, err := parseLyricsMode(*lyrics)
	if err != nil {
		log.Fatalf("lyrics: %v", err)
	}

	st, err := store.Open(*configDir, store.DefaultConfig(home))
	if err != nil {
		log.Fatalf("open settings in %s: %v", *configDir, err)
	}
	// Flags win over saved settings on every start, so the config file cannot
	// silently pin a directory the user has since changed on the command line.
	if err := st.Update(func(c *store.Config) {
		if flagPassed("dir") {
			c.Dir = *dir
		} else if c.Dir == "" {
			c.Dir = *dir
		}
		if flagPassed("output") {
			c.Output = *output
		} else if c.Output == "" {
			c.Output = *output
		}
		// The flag is clamped like the web API's copy of the setting, so the
		// file on disk never claims a concurrency the pool will not run.
		if flagPassed("workers") {
			c.Workers = job.ClampWorkers(*workers)
		} else if c.Workers <= 0 {
			c.Workers = job.ClampWorkers(*workers)
		}
		if flagPassed("lyrics") {
			c.Lyrics = mode
		}
		if flagPassed("workspace") {
			c.Workspace = *workspace
			c.Initialized = true
		} else if c.Workspace == "" {
			c.Workspace = store.DefaultWorkspace(home)
		}
	}); err != nil {
		log.Fatalf("save settings: %v", err)
	}
	cfg := st.Config()

	caches := &lyricCaches{}
	pool := job.New(cfg.Workers, makeProcessor(st, caches))

	// The lyrics backfill gets its own pool: starting a batch cancels whatever
	// that pool is already running, and a user who queues a backfill has not
	// asked to stop the conversion they started a moment ago. It also gets its
	// own concurrency, because a backfill of already-decoded files is mostly
	// waiting on the network while a conversion is mostly waiting on the disk.
	//
	// The searcher is shared with the web layer so that a manual search for a
	// song the batch has already looked up costs no request against an endpoint
	// that refuses a client after a handful of them.
	searcher := backfill.NewSearcher()
	lyricPool := job.New(cfg.Workers, backfill.Processor(backfill.Options{
		Store:    st,
		Clients:  caches.forWorkspace,
		Searcher: searcher,
		Log:      log.Printf,
	}))

	// The .lrc page and the translation page get pools of their own, for the
	// same reason once more: neither is a reason to stop the other.
	//
	// Both run at a lower concurrency than the conversion. The .lrc import
	// rewrites whole files, and a translation sends one request per song to a
	// paid endpoint — a phone firing sixteen of those at once is how a key gets
	// rate limited and a folder ends up half translated.
	slowWorkers := job.ClampWorkers((cfg.Workers + 1) / 2)
	lrcPool := job.New(slowWorkers, backfill.LRCProcessor(backfill.LRCOptions{Store: st}))
	aiPool := job.New(slowWorkers, backfill.AIProcessor(backfill.AIOptions{Store: st}))

	// Records are held for a couple of seconds rather than rewriting the file
	// once per track, so the end of a batch is when the last of them has to go
	// out. Shutdown below covers the rest.
	pool.OnIdle(func() {
		if err := st.Flush(); err != nil {
			log.Printf("save records: %v", err)
		}
	})

	srv := web.New(st, web.Pools{
		Convert: pool,
		Lyrics:  lyricPool,
		LRC:     lrcPool,
		AI:      aiPool,
	}, searcher, caches.forWorkspace, buildVersion)
	addr := fmt.Sprintf("%s:%d", *host, *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: the event stream and large downloads are both
		// long-lived by design.
	}

	log.Printf("ncm-studio %s", buildVersion)
	log.Printf("  scanning : %s", cfg.Dir)
	log.Printf("  output   : %s", cfg.Output)
	log.Printf("  workspace: %s", cfg.Workspace)
	log.Printf("  settings : %s", *configDir)
	log.Printf("  lyrics   : %s", lyricsModeName(cfg.Lyrics))
	if *host != "127.0.0.1" && *host != "localhost" {
		log.Printf("  note: bound to %s — anyone on this network can reach the UI", *host)
	}
	log.Printf("open http://%s", displayAddr(*host, *port))

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("server: %v", err)
	case <-stop:
		log.Println("shutting down")
	}

	pool.Cancel()
	lyricPool.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Whatever the write budget was still holding: a stopped batch never fires
	// the idle hook, and a settings change made seconds before Ctrl-C would
	// otherwise be the one thing lost.
	if err := st.Flush(); err != nil {
		log.Printf("save settings: %v", err)
	}
}

// buildVersion is stamped at build time with -ldflags "-X main.buildVersion=...".
var buildVersion = "dev"

func defaultConfigDir(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ncm-studio")
	}
	return filepath.Join(home, ".config", "ncm-studio")
}

// flagPassed reports whether a flag was given on the command line, which is
// how the tool tells "left at the default" from "deliberately set".
func flagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func parseLyricsMode(s string) (store.LyricsMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "no":
		return store.LyricsOff, nil
	case "embed", "tag", "tags":
		return store.LyricsEmbed, nil
	case "both", "all", "":
		return store.LyricsEmbedAndFile, nil
	case "file", "lrc", "sidecar", "file-only":
		return store.LyricsFileOnly, nil
	default:
		return store.LyricsOff, fmt.Errorf("lyrics: unknown mode %q (want off, embed, both or file)", s)
	}
}

func lyricsModeName(m store.LyricsMode) string {
	switch m {
	case store.LyricsOff:
		return "off"
	case store.LyricsEmbed:
		return "embed in tags"
	case store.LyricsFileOnly:
		return ".lrc sidecar only"
	default:
		return "embed in tags + .lrc sidecar"
	}
}

func displayAddr(host string, port int) string {
	switch host {
	case "0.0.0.0", "::", "":
		return fmt.Sprintf("<this-device-ip>:%d", port)
	case "127.0.0.1", "localhost":
		return fmt.Sprintf("localhost:%d", port)
	default:
		return fmt.Sprintf("%s:%d", host, port)
	}
}

// lyricCaches hands out one lyric client per cache directory.
//
// The cache lives in the workspace, and the workspace can be changed from the
// settings page while the program runs, so the client cannot be built once at
// startup. Caching by directory keeps a change from leaking a client per file.
type lyricCaches struct {
	mu     sync.Mutex
	dir    string
	client *lyric.Client
}

func (c *lyricCaches) forWorkspace(workspace string) *lyric.Client {
	dir := filepath.Join(workspace, "lyrics")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.dir != dir {
		c.client, c.dir = lyric.NewClient(dir), dir
	}
	return c.client
}

// makeProcessor adapts the pipeline to the job pool, adding the decisions the
// pool itself has no business making: whether to skip a file that is already
// decrypted, and recording the result afterwards.
func makeProcessor(st *store.Store, caches *lyricCaches) job.Processor {
	return func(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
		cfg := st.Config()

		hash, err := st.FingerprintCached(it.Path)
		if err != nil {
			return job.Result{}, fmt.Errorf("read file: %w", err)
		}
		if st.AlreadyDone(hash) {
			rec, _ := st.Lookup(hash)
			// Cached, not done: nothing was written this run, which is what keeps
			// the source file out of the offer to delete it afterwards.
			return job.Result{Output: rec.OutputPath, Lyrics: "ok", Cached: true}, nil
		}

		// A file may carry its own landing for the lyrics, chosen in the list
		// without moving the setting that governs the rest of the batch.
		mode := store.LyricsMode(cfg.Lyrics)
		if it.LyricsMode != nil {
			if m := store.LyricsMode(*it.LyricsMode); m.Valid() {
				mode = m
			}
		}

		// The source of a lyric is one setting for every path that reads one, so
		// a .ncm decrypted again is answered from the catalogue or from the
		// cache exactly as a backfill would be. A nil client is left alone:
		// pipeline.Process knows what to do without one.
		client := caches.forWorkspace(cfg.Workspace)
		if client != nil {
			client.SetRefresh(cfg.LyricsSource == store.LyricsSourceFetch)
		}

		res, err := pipeline.Process(ctx, it.Path, pipeline.Options{
			OutputDir: cfg.Output,
			Lyrics:    pipeline.LyricsMode(mode),
			Client:    client,
		}, func(p pipeline.Progress) {
			report(p.Stage, p.Done, p.Total)
		})
		if err != nil {
			return job.Result{}, err
		}

		lyricState := "none"
		if res.Lyrics != nil {
			lyricState = "ok"
		} else if len(res.Warnings) > 0 {
			lyricState = "failed"
		}

		title := it.Name
		var musicID int64
		if res.Meta != nil {
			if name := res.Meta.DisplayName(); name != "" {
				title = name
			}
			musicID = res.Meta.MusicID
		}
		if err := st.Record(store.Record{
			SourceHash: hash,
			SourcePath: it.Path,
			OutputPath: res.OutputPath,
			Title:      title,
			MusicID:    musicID,
			Lyrics:     res.Lyrics != nil,
			At:         time.Now(),
		}); err != nil {
			log.Printf("record %s: %v", it.Name, err)
		}

		// The source is deleted only when it was actually decrypted just now,
		// never on the path above that short-circuits a file already done —
		// that branch runs before anything has been read, and removing a file
		// on the strength of a record rather than of an output written in this
		// run is not what "delete after decrypting" says. A failure to remove
		// is a warning on the row rather than a failed job: the audio is on
		// disk and correct, and that is the part that matters.
		removed := false
		var warnings []string
		warnings = append(warnings, res.Warnings...)
		if cfg.DeleteSource == store.DeleteAuto {
			if err := os.Remove(it.Path); err != nil {
				log.Printf("delete source %s: %v", it.Name, err)
				warnings = append(warnings, "the source file could not be deleted: "+err.Error())
			} else {
				removed = true
			}
		}

		return job.Result{
			Output:   res.OutputPath,
			LRC:      res.LRCPath,
			Lyrics:   lyricState,
			Warnings: warnings,
			Removed:  removed,
		}, nil
	}
}
