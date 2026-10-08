package arcade

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var errStaleCursor = errors.New("directory changed; reload its first page")

type directoryCursor struct{ Server, Path, Generation, After string }
type listingItem struct {
	key   string
	entry FileEntry
}
type listingHeap []listingItem

func (h listingHeap) Len() int           { return len(h) }
func (h listingHeap) Less(i, j int) bool { return h[i].key > h[j].key }
func (h listingHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *listingHeap) Push(v any)        { *h = append(*h, v.(listingItem)) }
func (h *listingHeap) Pop() any          { a := *h; v := a[len(a)-1]; *h = a[:len(a)-1]; return v }
func directoryGeneration(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("directory identity unavailable")
	}
	return fmt.Sprintf("%d:%d:%d", sys.Dev, sys.Ino, st.ModTime().UnixNano()), nil
}

// Keep only the smallest page following the cursor in memory, independent of
// directory size. Cursors bind identity+generation+scope; a changed directory
// is explicit conflict rather than silent duplication/omission between pages.
func (m *Manager) ListFilesPage(ctx context.Context, s *Server, rel, cursor string) ([]FileEntry, string, error) {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if err := m.requireRegistered(s); err != nil {
		return nil, "", err
	}
	root, name, err := m.rooted(s, rel)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	d, err := openDirIn(root, name)
	if err != nil {
		return nil, "", err
	}
	defer d.Close()
	generation, err := directoryGeneration(d)
	if err != nil {
		return nil, "", err
	}
	after := ""
	if cursor != "" {
		if len(cursor) > 4096 {
			return nil, "", fmt.Errorf("invalid directory cursor")
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", err
		}
		var c directoryCursor
		if json.Unmarshal(b, &c) != nil || c.Server != s.ID || c.Path != name {
			return nil, "", fmt.Errorf("cursor belongs to a different directory")
		}
		if c.Generation != generation {
			return nil, "", errStaleCursor
		}
		after = c.After
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	page := &listingHeap{}
	heap.Init(page)
	scanned := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		entries, rerr := d.ReadDir(256)
		for _, entry := range entries {
			scanned++
			if scanned > 1_000_000 {
				return nil, "", fmt.Errorf("directory exceeds one million entries; use a narrower directory")
			}
			prefix := "1:"
			if entry.IsDir() {
				prefix = "0:"
			}
			key := prefix + strings.ToLower(entry.Name()) + "\x00" + entry.Name()
			if key <= after {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			item := listingItem{key, FileEntry{Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(name, entry.Name())), Dir: entry.IsDir(), Size: info.Size(), Mod: info.ModTime().Unix(), Text: !entry.IsDir() && isTextFile(entry.Name(), info.Size()), Limit: !entry.IsDir() && info.Size() > maxEditBytes}}
			if page.Len() < maxListItems+1 {
				heap.Push(page, item)
			} else if key < (*page)[0].key {
				heap.Pop(page)
				heap.Push(page, item)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, "", rerr
		}
	}
	current, err := directoryGeneration(d)
	if err != nil {
		return nil, "", err
	}
	if current != generation {
		return nil, "", errStaleCursor
	}
	sort.Slice(*page, func(i, j int) bool { return (*page)[i].key < (*page)[j].key })
	more := page.Len() > maxListItems
	if more {
		*page = (*page)[:maxListItems]
	}
	out := make([]FileEntry, 0, page.Len())
	for _, item := range *page {
		out = append(out, item.entry)
	}
	next := ""
	if more {
		b, _ := json.Marshal(directoryCursor{s.ID, name, generation, (*page)[page.Len()-1].key})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, next, nil
}
