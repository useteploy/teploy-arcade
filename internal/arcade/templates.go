package arcade

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
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

// seedLedger records the hash of every template file the panel wrote itself.
const seedLedger = ".seeded.json"

// syncSeededTemplates keeps the panel's own templates current without ever
// overwriting one an operator has edited.
//
// The directory used to be seeded once, when it was empty, and never looked at
// again - so a template shipped in an upgrade could not reach a panel that had
// already been installed. That is not theoretical: v0.18.0 fixed Bedrock's dead
// pinned version, added UDP publishing for Bedrock, Rust and Valheim, and gave
// each a ready banner, and every one of those fixes was inert on the deployed
// panel because `bedrock.json` on its disk was still the snapshot written at
// first run. A fix that cannot reach the installation it was written for is not
// a fix.
//
// So the panel records what it wrote. A file whose bytes still match what the
// panel put there is the panel's, and is replaced with the current version; a
// file that differs is the operator's, and is left exactly alone. Templates are
// meant to be edited - that is the whole point of them being files - and an
// upgrade silently reverting an edit would be a far worse bug than the one this
// fixes.
//
// A panel installed before the ledger existed has no record either way. Those
// files are refreshed, but the old one is kept beside it as
// `<slug>.json.superseded` rather than discarded, because "the panel could not
// tell" is not grounds for destroying an edit that might be there.
func syncSeededTemplates(dir string) error {
	ledgerPath := filepath.Join(dir, seedLedger)
	known := map[string]string{}
	hadLedger := false
	if b, err := os.ReadFile(ledgerPath); err == nil {
		if json.Unmarshal(b, &known) == nil {
			hadLedger = true
		}
	}

	next := map[string]string{}
	for _, t := range tplBuiltin {
		b, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return err
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(b))
		next[t.Slug] = sum

		path := filepath.Join(dir, t.Slug+".json")
		cur, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			// New template in this build, or a first run.

		case err != nil:
			return err

		case fmt.Sprintf("%x", sha256.Sum256(cur)) == known[t.Slug]:
			// The panel wrote this and nobody has touched it. Nothing to keep.

		case fmt.Sprintf("%x", sha256.Sum256(cur)) == sum:
			// Already current; do not rewrite it just to update the ledger's
			// idea of it.
			continue

		case !hadLedger:
			aside := path + ".superseded"
			if err := os.WriteFile(aside, cur, 0o644); err != nil {
				return err
			}
			log.Printf("template %s: refreshed to the version shipped with this build; "+
				"the previous file was kept at %s", t.Slug, filepath.Base(aside))

		default:
			// Edited on purpose. It wins, and the ledger keeps pointing at what
			// the panel last wrote so a later edit-revert is still detectable.
			next[t.Slug] = known[t.Slug]
			continue
		}

		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}

	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ledgerPath, b, 0o600)
}

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

	if err := syncSeededTemplates(dir); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var loaded []Template
	var bad []string
	for _, e := range ents {
		// Dotfiles are the panel's own bookkeeping, not templates. The seed
		// ledger is `.seeded.json` and would otherwise be read as a template
		// with no slug, failing the whole load with a validation error about a
		// file the operator never created.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
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

	// Group order is stable and intentional: Minecraft first, then proxies,
	// then the other games.
	//
	// The group used to be called "Playable Server", which stopped meaning
	// anything once it held Bedrock and not Terraria: Terraria, Rust and CS2
	// are all playable servers too, so the label described nothing and the
	// split it implied was between Minecraft and everything else all along.
	// Both editions of Minecraft belong together; a different game does not
	// belong among Minecraft's server flavours.
	rank := map[string]int{"Minecraft": 0, "Network Proxy": 1, "Other": 2}
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
