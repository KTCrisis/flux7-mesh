package policy

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/fsnotify/fsnotify"
)

// Watcher monitors config and policy files for changes and triggers a reload
// callback with the new policies. Validation failures are logged and the
// current policies are kept — the mesh never crashes on a bad reload.
type Watcher struct {
	configPath string
	fsw        *fsnotify.Watcher
	onReload   func([]config.Policy)
	done       chan struct{}
}

// NewWatcher starts watching configPath (and the policy_dir it references)
// for changes. On each valid change, onReload is called with the new policies.
func NewWatcher(configPath string, onReload func([]config.Policy)) (*Watcher, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := fsw.Add(filepath.Dir(absConfig)); err != nil {
		fsw.Close()
		return nil, err
	}

	_, policyDir, _ := config.LoadPolicies(absConfig)
	if policyDir != "" {
		if err := fsw.Add(policyDir); err != nil {
			slog.Warn("policy watcher: cannot watch policy_dir", "path", policyDir, "error", err)
		}
	}

	w := &Watcher{
		configPath: absConfig,
		fsw:        fsw,
		onReload:   onReload,
		done:       make(chan struct{}),
	}

	go w.loop(policyDir)
	return w, nil
}

func (w *Watcher) loop(policyDir string) {
	var debounce *time.Timer

	for {
		select {
		case <-w.done:
			if debounce != nil {
				debounce.Stop()
			}
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !isRelevant(ev, w.configPath, policyDir) {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(200*time.Millisecond, func() {
				w.reload()
			})

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Error("policy watcher error", "error", err)
		}
	}
}

func isRelevant(ev fsnotify.Event, configPath, policyDir string) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	abs, err := filepath.Abs(ev.Name)
	if err != nil {
		return false
	}
	if abs == configPath {
		return true
	}
	if policyDir != "" && filepath.Dir(abs) == policyDir && filepath.Ext(abs) == ".yaml" {
		return true
	}
	return false
}

func (w *Watcher) reload() {
	policies, _, err := config.LoadPolicies(w.configPath)
	if err != nil {
		slog.Error("policy hot-reload failed, keeping current policies", "error", err)
		return
	}
	slog.Info("policy hot-reload", "policies", len(policies))
	w.onReload(policies)
}

func (w *Watcher) Close() {
	close(w.done)
	w.fsw.Close()
}
