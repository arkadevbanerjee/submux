// reload.go lets a running relay pick up config.json edits without a restart.
// A restart used to be the only way, and forgetting it meant edits silently did
// nothing while the screen looked fine.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const reloadPollEvery = 2 * time.Second

// reloadConfig loads path, resolves its credentials and swaps it in. On any
// error the running config is left untouched.
func (s *server) reloadConfig(path string) error {
	next, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := resolveCredentials(next); err != nil {
		return err
	}
	next.Listen = s.conf().Listen // the listener is already bound; a changed address needs a restart
	s.cfgp.Store(next)
	return nil
}

// watchConfig reloads on SIGHUP and whenever the file's mtime changes. A bad
// edit is logged once and ignored until the file changes again.
func (s *server) watchConfig(path string, every time.Duration) {
	var last time.Time
	if fi, err := os.Stat(path); err == nil {
		last = fi.ModTime()
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		force := false
		select {
		case <-hup:
			force = true
		case <-tick.C:
		}
		fi, err := os.Stat(path)
		if err != nil || (!force && fi.ModTime().Equal(last)) {
			continue
		}
		last = fi.ModTime()
		if err := s.reloadConfig(path); err != nil {
			log.Printf("submux: config reload FAILED, keeping the previous config: %v", err)
			continue
		}
		log.Printf("submux: config reloaded from %s: %s", path, describeReload(s.conf()))
	}
}

func describeReload(c *config) string {
	return fmt.Sprintf("%d route(s), default fallback %v", len(c.Routes), c.DefaultFallback)
}
