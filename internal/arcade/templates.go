package arcade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Phase 6: templates as data, not code.
//
// This resolves the shape half of PLAN.md §12 question 2. The format is
// clean-slate rather than Pterodactyl's egg JSON, because the thing the panel
// actually needs from a template is *display metadata* alongside the runtime
// bits - group, human description, recommended flag, maturity, per-field help.
// Ptero's format carries none of that; it carries an installer script and a
// Docker image, and its licence/complexity come along for the ride.
//
// A template is one JSON file in data/templates/. Dropping a file in and
// restarting is the whole installation story, which is what Phase 6's DoD
// ("install a new game from the registry, no code changes required") means.

var (
	tplMu      sync.RWMutex
	tplLoaded  []Template
	tplBuiltin = templates // the compiled-in set, used to seed an empty dir
)

func templatesDir(dataDir string) string { return filepath.Join(dataDir, "templates") }

// LoadTemplates reads data/templates/*.json, seeding it from the built-in set
// on first run so the directory is discoverable and editable.
func LoadTemplates(dataDir string) error {
	dir := templatesDir(dataDir)
	// 0o700 to match the data dir above it. A template file names the image a
	// game server is launched from, so another account being able to read the
	// directory is the first half of being able to swap one.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	seeded := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			seeded++
		}
	}
	if seeded == 0 {
		for _, t := range tplBuiltin {
			b, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, t.Slug+".json"), b, 0o644); err != nil {
				return err
			}
		}
		ents, err = os.ReadDir(dir)
		if err != nil {
			return err
		}
	}

	var loaded []Template
	var bad []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			bad = append(bad, e.Name()+": "+err.Error())
			continue
		}
		var t Template
		if err := json.Unmarshal(b, &t); err != nil {
			bad = append(bad, e.Name()+": "+err.Error())
			continue
		}
		if err := validateTemplate(&t); err != nil {
			bad = append(bad, e.Name()+": "+err.Error())
			continue
		}
		loaded = append(loaded, t)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("no usable templates in %s (%s)", dir, strings.Join(bad, "; "))
	}

	// Group order is stable and intentional: playable servers first, then
	// proxies, then everything else.
	rank := map[string]int{"Playable Server": 0, "Network Proxy": 1, "Other": 2}
	sort.SliceStable(loaded, func(i, j int) bool {
		ri, okI := rank[loaded[i].Group]
		rj, okJ := rank[loaded[j].Group]
		if !okI {
			ri = 3
		}
		if !okJ {
			rj = 3
		}
		if ri != rj {
			return ri < rj
		}
		return loaded[i].Name < loaded[j].Name
	})

	tplMu.Lock()
	tplLoaded = loaded
	tplMu.Unlock()

	if len(bad) > 0 {
		return fmt.Errorf("%d template(s) skipped: %s", len(bad), strings.Join(bad, "; "))
	}
	return nil
}

func validateTemplate(t *Template) error {
	switch {
	case t.Slug == "":
		return fmt.Errorf("slug is required")
	case t.Name == "":
		return fmt.Errorf("name is required")
	case t.Image == "":
		return fmt.Errorf("image is required")
	case len(t.Versions) == 0:
		return fmt.Errorf("at least one version is required")
	case t.MemoryMB <= 0:
		return fmt.Errorf("memory_mb must be positive")
	case t.CPU <= 0:
		return fmt.Errorf("cpu must be positive")
	}
	if t.Group == "" {
		t.Group = "Other"
	}
	if t.Mark == "" {
		t.Mark = "vanilla"
	}
	if t.Maturity == "" {
		t.Maturity = "stable"
	}
	if t.MaxPlayers <= 0 {
		t.MaxPlayers = 20
	}
	if t.DiskGB <= 0 {
		t.DiskGB = 10
	}
	if t.PortHint <= 0 {
		t.PortHint = 25565
	}
	return nil
}

func allTemplates() []Template {
	tplMu.RLock()
	defer tplMu.RUnlock()
	if len(tplLoaded) == 0 {
		return tplBuiltin
	}
	out := make([]Template, len(tplLoaded))
	copy(out, tplLoaded)
	return out
}

// templateBySlug resolves against the on-disk set, falling back to built-ins.
func templateBySlugLoaded(slug string) *Template {
	for _, t := range allTemplates() {
		if t.Slug == slug {
			c := t
			return &c
		}
	}
	return nil
}
