package arcade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Clients opt into the durable 202 resource with Prefer: respond-async.
// Legacy synchronous callers remain compatible during migration.
func (m *Manager) startOperation(s *Server, kind, actor, key, request string, work func(context.Context) error) (ImportJob, error) {
	if key == "" || len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
		return ImportJob{}, fmt.Errorf("async requests require an Idempotency-Key of 1–128 characters")
	}
	sum := sha256.Sum256([]byte(actor + "\x00" + s.ID + "\x00" + kind + "\x00" + key))
	id := hex.EncodeToString(sum[:16])
	payload := sha256.Sum256([]byte(request))
	fingerprint := hex.EncodeToString(payload[:])
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if existing, ok := importJobByID(id); ok {
		v := existing.view()
		if v.RequestHash != fingerprint {
			return ImportJob{}, fmt.Errorf("Idempotency-Key was already used with different request content")
		}
		return v, nil
	}
	if err := m.requireRegistered(s); err != nil {
		return ImportJob{}, err
	}
	job := newImportJob(&ImportScan{Path: m.serverDir(s), Name: s.Name}, kind, s.Name)
	importJobs.mu.Lock()
	delete(importJobs.jobs, job.pub.ID)
	job.pub.ID = id
	job.pub.ServerID = s.ID
	job.pub.RequestHash = fingerprint
	importJobs.jobs[id] = job
	importJobs.mu.Unlock()
	job.ctx = m.ctx
	if err := m.trackImportJob(job); err != nil {
		return ImportJob{}, err
	}
	if !m.own(func() {
		defer func() {
			if value := recover(); value != nil {
				job.fail(fmt.Errorf("operation interrupted by internal panic"))
			}
		}()
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
		defer cancel()
		if err := work(ctx); err != nil {
			job.fail(err)
		} else {
			job.done(s.ID)
		}
	}) {
		job.fail(context.Canceled)
		return ImportJob{}, context.Canceled
	}
	return job.view(), nil
}
func wantsAsync(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Prefer")), "respond-async")
}
func (a *API) operationStatus(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	job, ok := importJobByID(r.PathValue("job"))
	if !ok || job.view().ServerID != s.ID {
		writeErr(w, 404, fmt.Errorf("no such operation"))
		return
	}
	view := job.view()
	if view.Mode != "backup" && view.Mode != "plugin-install" && view.Mode != "restore" {
		writeErr(w, 404, fmt.Errorf("no such operation"))
		return
	}
	writeJSON(w, 200, publicOperation(view))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func publicOperation(view ImportJob) ImportJob { view.Path = ""; view.RequestHash = ""; return view }
