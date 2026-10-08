package arcade

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const durableJobLimit = 1000
const durableJobBytes = 64 << 10

// Serialize admission, terminal publication and recovery so an older snapshot
// cannot overwrite a terminal result, or retention race a new admission.
var jobStoreMu sync.Mutex

func jobRecordName(id string) (string, error) {
	if id == "" || len(id) > 128 {
		return "", fmt.Errorf("invalid job ID")
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", fmt.Errorf("invalid job ID")
		}
	}
	return id + ".json", nil
}

func openJobRoot(dataDir string) (*os.Root, string, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, "", err
	}
	base, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, "", err
	}
	defer base.Close()
	if err := base.Mkdir("jobs", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, "", err
	}
	info, err := base.Lstat("jobs")
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("jobs must be a real directory; preserve and inspect %s", filepath.Join(dataDir, "jobs"))
	}
	root, err := base.OpenRoot("jobs")
	path, absErr := filepath.Abs(filepath.Join(dataDir, "jobs"))
	if absErr != nil {
		if root != nil {
			root.Close()
		}
		return nil, "", absErr
	}
	return root, path, err
}

func validateJob(v ImportJob, name string) error {
	expected, err := jobRecordName(v.ID)
	if err != nil || expected != name {
		return fmt.Errorf("job ID does not match filename")
	}
	if v.Mode != ImportCopy && v.Mode != ImportAdopt && v.Mode != "clone" && v.Mode != "backup" && v.Mode != "plugin-install" && v.Mode != "restore" {
		return fmt.Errorf("unknown job mode")
	}
	if v.Name == "" || !filepath.IsAbs(v.Path) || v.Started <= 0 || v.Total < 0 || v.Copied < 0 || v.Skipped < 0 || v.Percent < 0 || v.Percent > 100 {
		return fmt.Errorf("invalid job identity, timestamp or counters")
	}
	if v.RequestHash != "" {
		hash, err := hex.DecodeString(v.RequestHash)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("invalid request fingerprint")
		}
	}
	switch v.State {
	case ImportRunning:
		if v.Finished != 0 || ((v.Mode == ImportCopy || v.Mode == ImportAdopt || v.Mode == "clone") && v.ServerID != "") || v.Error != "" {
			return fmt.Errorf("running job has a terminal result")
		}
	case ImportDone:
		if v.Finished < v.Started || v.ServerID == "" || v.Error != "" {
			return fmt.Errorf("invalid completed job")
		}
	case ImportFailed:
		if v.Finished < v.Started || v.Error == "" {
			return fmt.Errorf("invalid failed job")
		}
	default:
		return fmt.Errorf("unknown job state")
	}
	return nil
}

func readJobRecords(root *os.Root) (map[string]ImportJob, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(durableJobLimit + 1)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > durableJobLimit {
		return nil, fmt.Errorf("job registry exceeds %d records; preserve and archive reviewed terminal records before retrying", durableJobLimit)
	}
	records := make(map[string]ImportJob, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("job record %s: %w", name, err)
		}
		if !strings.HasSuffix(name, ".json") || !info.Mode().IsRegular() || info.Size() > durableJobBytes {
			return nil, fmt.Errorf("job record %s is not a bounded regular JSON file; preserve it for inspection", name)
		}
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("job record %s: %w", name, err)
		}
		actual, statErr := file.Stat()
		if statErr != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
			file.Close()
			return nil, fmt.Errorf("job record %s changed or is not regular; preserve it for inspection", name)
		}
		b, readErr := io.ReadAll(io.LimitReader(file, durableJobBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(b) > durableJobBytes {
			return nil, fmt.Errorf("job record %s cannot be read within 64 KiB; preserve it for inspection", name)
		}
		// Reject duplicate fields as well as unknown fields and trailing values.
		var keys map[string]json.RawMessage
		decoder := json.NewDecoder(bytes.NewReader(b))
		token, tokenErr := decoder.Token()
		if tokenErr != nil || token != json.Delim('{') {
			return nil, fmt.Errorf("job record %s must be a JSON object; preserve it for inspection", name)
		}
		keys = map[string]json.RawMessage{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("corrupt job record %s; preserve it for inspection", name)
			}
			field, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("corrupt job record %s; preserve it for inspection", name)
			}
			if _, duplicate := keys[field]; duplicate {
				return nil, fmt.Errorf("duplicate field in job record %s; preserve it for inspection", name)
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return nil, fmt.Errorf("corrupt job record %s; preserve it for inspection", name)
			}
			keys[field] = value
		}
		var v ImportJob
		decoder = json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&v); err != nil {
			return nil, fmt.Errorf("corrupt job record %s: %w; preserve it for inspection", name, err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("trailing JSON in job record %s; preserve it for inspection", name)
		}
		if err := validateJob(v, name); err != nil {
			return nil, fmt.Errorf("invalid job record %s: %w; preserve it for inspection", name, err)
		}
		records[name] = v
	}
	return records, nil
}

func pruneJobRecords(root *os.Root, records map[string]ImportJob) error {
	cutoff := time.Now().Add(-importJobTTL).Unix()
	removed := false
	for name, v := range records {
		if v.State != ImportRunning && v.Finished < cutoff {
			if err := root.Remove(name); err != nil {
				return fmt.Errorf("expire terminal job %s: %w", name, err)
			}
			delete(records, name)
			removed = true
		}
	}
	if removed {
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
	}
	return nil
}

func (m *Manager) trackImportJob(job *importJob) (err error) {
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()
	// newImportJob creates a provisional memory entry. Failed durable admission
	// must not leave a pollable operation that never started.
	defer func() {
		if err != nil {
			importJobs.mu.Lock()
			if importJobs.jobs[job.pub.ID] == job {
				delete(importJobs.jobs, job.pub.ID)
			}
			importJobs.mu.Unlock()
		}
	}()
	root, dir, err := openJobRoot(m.dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	records, err := readJobRecords(root)
	if err != nil {
		return err
	}
	if err := pruneJobRecords(root, records); err != nil {
		return err
	}
	if len(records) >= durableJobLimit {
		return fmt.Errorf("job registry is full (%d); wait for terminal records to expire or archive reviewed metadata", durableJobLimit)
	}
	view := job.view()
	name, err := jobRecordName(view.ID)
	if err != nil {
		return err
	}
	if _, exists := records[name]; exists {
		return fmt.Errorf("job ID already exists; existing record preserved")
	}
	if err := validateJob(view, name); err != nil {
		return err
	}
	b, err := json.Marshal(view)
	if err != nil {
		return err
	}
	if len(b) > durableJobBytes {
		return fmt.Errorf("job metadata exceeds 64 KiB")
	}
	path := filepath.Join(dir, name)
	if err := writeDurable(path, b); err != nil {
		return fmt.Errorf("persist initial job: %w", err)
	}
	job.mu.Lock()
	job.storePath = path
	job.mu.Unlock()
	return nil
}

func (job *importJob) persist() error {
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.storePath == "" {
		return nil
	} // Existing isolated in-memory fixtures.
	view := job.pub
	view.Copied = job.copied.Load()
	if view.State == ImportDone {
		view.Percent = 100
	} else if view.Total > 0 {
		if view.Copied >= view.Total {
			view.Percent = 99
		} else {
			view.Percent = int(float64(view.Copied) / float64(view.Total) * 100)
		}
	}
	if err := validateJob(view, filepath.Base(job.storePath)); err != nil {
		return err
	}
	b, err := json.Marshal(view)
	if err != nil {
		return err
	}
	if len(b) > durableJobBytes {
		return fmt.Errorf("job metadata exceeds 64 KiB; previous record preserved")
	}
	return writeDurable(job.storePath, b)
}

func (m *Manager) loadImportJobs() error {
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()
	root, dir, err := openJobRoot(m.dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	records, err := readJobRecords(root)
	if err != nil {
		return fmt.Errorf("cannot recover job registry in %s: %w", dir, err)
	}
	if err := pruneJobRecords(root, records); err != nil {
		return err
	}
	recovered := map[string]*importJob{}
	for name, view := range records {
		if view.State == ImportRunning {
			view.State = ImportFailed
			view.Error = "Operation interrupted by panel restart; partial data is preserved. Inspect the source and managed server directories before retrying."
			view.Finished = max(view.Started, time.Now().Unix())
			b, err := json.Marshal(view)
			if err != nil {
				return err
			}
			if len(b) > durableJobBytes {
				return fmt.Errorf("interrupted job %s cannot be recorded within 64 KiB; preserve its original record for inspection", name)
			}
			if err := writeDurable(filepath.Join(dir, name), b); err != nil {
				return fmt.Errorf("persist interrupted job %s: %w", name, err)
			}
		}
		job := &importJob{ctx: m.ctx, pub: view, storePath: filepath.Join(dir, name)}
		job.copied.Store(view.Copied)
		recovered[view.ID] = job
	}
	importJobs.mu.Lock()
	defer importJobs.mu.Unlock()
	for id, job := range importJobs.jobs {
		job.mu.Lock()
		owned := filepath.Dir(job.storePath) == dir
		job.mu.Unlock()
		if owned {
			delete(importJobs.jobs, id)
		}
	}
	for id, job := range recovered {
		importJobs.jobs[id] = job
	}
	return nil
}

// reapImportJobs is safe for the application's owned periodic worker. Unlike
// boot recovery it never classifies a still-running operation as interrupted.
func (m *Manager) reapImportJobs() error {
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()
	root, dir, err := openJobRoot(m.dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	records, err := readJobRecords(root)
	if err != nil {
		return fmt.Errorf("cannot expire job metadata in %s: %w", dir, err)
	}
	if err := pruneJobRecords(root, records); err != nil {
		return err
	}
	importJobs.mu.Lock()
	defer importJobs.mu.Unlock()
	for id, job := range importJobs.jobs {
		job.mu.Lock()
		owned := filepath.Dir(job.storePath) == dir
		terminal := job.pub.State != ImportRunning
		job.mu.Unlock()
		if owned && terminal {
			if _, retained := records[id+".json"]; !retained {
				delete(importJobs.jobs, id)
			}
		}
	}
	return nil
}
