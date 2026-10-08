package arcade

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type restoreJournal struct {
	Phase         string            `json:"phase"`
	Original      []string          `json:"original"`
	New           []string          `json:"new"`
	OldMaxPlayers int               `json:"old_max_players,omitempty"`
	OldPort       int               `json:"old_port"`
	OldProps      map[string]string `json:"old_props"`
}

// Transaction phase boundaries require durable bytes and directory entries.
// No caller interprets a post-publication sync failure as 'nothing changed'.
func writeRestoreJournal(staging string, j restoreJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(staging, "journal.json"), b)
}
func writeDurable(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".arcade-tmp-journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func rollbackRestoreJournal(installed []string, held, dir, staging string, journal restoreJournal) error {
	journal.Phase = "rolling_back"
	if err := writeRestoreJournal(staging, journal); err != nil {
		return err
	}
	return rollbackRestore(installed, held, dir)
}

func syncDirectories(paths ...string) error {
	for _, path := range paths {
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		err = errors.Join(d.Sync(), d.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func validateRestoreJournal(j restoreJournal) error {
	if j.Phase != "evacuating" && j.Phase != "installing" && j.Phase != "rolling_back" {
		return errors.New("unknown restore phase")
	}
	if j.OldProps == nil || j.OldPort < 1 || j.OldPort > 65535 || j.OldMaxPlayers < 0 {
		return errors.New("invalid restore metadata")
	}
	if len(j.Original) > 100000 || len(j.New) > 100000 {
		return errors.New("restore namespace manifest exceeds bound")
	}
	for _, names := range [][]string{j.Original, j.New} {
		seen := map[string]bool{}
		for _, name := range names {
			if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") || seen[name] {
				return errors.New("invalid restore namespace manifest")
			}
			seen[name] = true
		}
	}
	return nil
}
func readRestoreJournalIn(root *os.Root, stage string) (restoreJournal, bool, error) {
	var j restoreJournal
	f, st, err := openRegularIn(root, filepath.Join(stage, "journal.json"))
	if os.IsNotExist(err) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	defer f.Close()
	if st.Size() > 8<<20 {
		return j, true, errors.New("restore journal exceeds 8 MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, (8<<20)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return j, true, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return j, true, errors.New("trailing restore journal data")
	}
	return j, true, validateRestoreJournal(j)
}
func readDirectoryIn(root *os.Root, name string) ([]os.DirEntry, error) {
	d, err := openDirIn(root, name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(100001)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 100000 {
		return nil, errors.New("restore directory exceeds 100000-entry bound")
	}
	return entries, nil
}
func restoreHeldIn(root *os.Root, held string) error {
	entries, err := readDirectoryIn(root, held)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.Rename(filepath.Join(held, entry.Name()), entry.Name()); err != nil {
			return err
		}
	}
	return syncDirectoriesRoot(root, ".", held)
}
func syncDirectoriesRoot(root *os.Root, paths ...string) error {
	for _, path := range paths {
		d, err := openDirIn(root, path)
		if err != nil {
			return err
		}
		err = errors.Join(d.Sync(), d.Close())
		if err != nil {
			return err
		}
	}
	return nil
}
func writeRestoreJournalIn(root *os.Root, stage string, j restoreJournal) error {
	if err := validateRestoreJournal(j); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	id, err := randomHex(16)
	if err != nil {
		return err
	}
	tmp := filepath.Join(stage, ".arcade-tmp-journal-"+id)
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = root.Rename(tmp, filepath.Join(stage, "journal.json")); err != nil {
		return err
	}
	return syncDirectoriesRoot(root, stage)
}
