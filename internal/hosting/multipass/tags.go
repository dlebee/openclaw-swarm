package multipass

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// tagStore persists per-instance tag lists in a local sidecar directory
// (default: ~/.cache/openclaw/multipass/tags). Multipass has no tag
// primitive — `claws destroy` and `create-machine`'s existence probe both
// rely on `ListByTag("claws/<prefix>")`, so we emulate the tag surface in
// a JSON file per VM label.
//
// This is best-effort, local, and cheap: we're fine if tag writes fail in
// extreme cases (full disk, permission denied) because the provider always
// re-reads the file on next boot. It's NOT shared across hosts — a VM
// launched from machine A is invisible to machine B, which matches how
// Multipass itself works.
type tagStore struct {
	dir string
}

// newTagStore resolves the on-disk tag directory. Passing empty uses
// $XDG_CACHE_HOME/openclaw/multipass/tags (default
// $HOME/.cache/openclaw/multipass/tags). Errors here are fatal — there's
// no recovery from "can't determine cache directory".
func newTagStore(dir string) (*tagStore, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolve cache dir: %w", err)
		}
		dir = filepath.Join(base, "openclaw", "multipass", "tags")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tag dir %s: %w", dir, err)
	}
	return &tagStore{dir: dir}, nil
}

// path returns the sidecar path for a given VM label. Labels are sanitized
// elsewhere; we don't re-sanitize here.
func (s *tagStore) path(label string) string {
	return filepath.Join(s.dir, label+".json")
}

// save writes the deduplicated, sorted tag set for a label. A nil/empty
// tag list removes the sidecar instead of writing an empty file, so the
// listing-by-tag path doesn't churn through pseudo-entries.
func (s *tagStore) save(label string, tags []string) error {
	tags = dedupSorted(tags)
	if len(tags) == 0 {
		return s.forget(label)
	}
	data, err := json.MarshalIndent(tags, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(label) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path(label)); err != nil {
		return fmt.Errorf("commit %s: %w", s.path(label), err)
	}
	return nil
}

// load returns the tag set for a label, or an empty slice when no sidecar
// exists. Missing-sidecar is not an error because a VM launched outside
// our flow (by a developer via the multipass CLI directly) simply has no
// tags, which is the correct semantic — it won't show up in ListByTag.
func (s *tagStore) load(label string) ([]string, error) {
	data, err := os.ReadFile(s.path(label))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var tags []string
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, fmt.Errorf("decode %s: %w", s.path(label), err)
	}
	return tags, nil
}

// forget removes the sidecar. Called on DeleteInstance so a label can be
// reused without stale tags leaking into the next ListByTag walk.
func (s *tagStore) forget(label string) error {
	err := os.Remove(s.path(label))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// matchesTag is a linear scan; tag lists on these labels are small
// (handful of claws/* entries) so O(n) is fine.
func matchesTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
