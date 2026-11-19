package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hayfever/mayfly/internal/bundle"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/version"
)

// finalize exports the artifact bundle, reports completion, and returns the
// first error encountered. It runs at most once.
func (r *Runtime) finalize(ctx context.Context, reason string, runErr error) error {
	r.finalizeMu.Lock()
	defer r.finalizeMu.Unlock()
	if r.finalized {
		return r.finalErr
	}
	r.finalized = true
	r.finalErr = r.doFinalize(ctx, reason, runErr)
	return r.finalErr
}

func (r *Runtime) doFinalize(ctx context.Context, reason string, runErr error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	r.logger.Info("finalizing session", "reason", reason)
	r.events.Emit(ctx, model.EventStatus, "mayfly", "Session closing.", model.StatusData{Status: model.StatusClosing, Reason: reason})

	if err := r.conv.Close(); err != nil {
		r.logger.Warn("close conversation log", "error", err)
	}
	if err := r.trace.Close(); err != nil {
		r.logger.Warn("close trace log", "error", err)
	}
	if err := r.events.Close(); err != nil {
		r.logger.Warn("close event log", "error", err)
	}

	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	manifest := bundle.Manifest{
		Version:      1,
		SessionID:    r.cfg.SessionID,
		Title:        r.cfg.Title,
		Agent:        r.cfg.Agent,
		Harness:      r.cfg.Harness,
		Model:        r.cfg.Model,
		Status:       status,
		Reason:       reason,
		CreatedAt:    r.cfg.CreatedAt,
		FinishedAt:   time.Now().UTC(),
		AgentVersion: version.Version,
		MessageCount: int(r.messages.Load()),
		ToolCalls:    int(r.toolCalls.Load()),
		Partial:      runErr != nil,
	}
	for _, s := range r.cfg.Skills {
		manifest.Skills = append(manifest.Skills, s.Name)
	}
	for _, s := range r.cfg.MCPServers {
		manifest.MCPServers = append(manifest.MCPServers, s.Name)
	}
	for _, p := range r.cfg.Plugins {
		manifest.Plugins = append(manifest.Plugins, p.Name)
	}

	cfgJSON, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return r.failFinalize(ctx, "encode session config: "+err.Error())
	}

	finalPath := filepath.Join(r.cfg.ArtifactsDir, "bundle.tar.gz")
	tmpPath := finalPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return r.failFinalize(ctx, "create bundle: "+err.Error())
	}
	hasher := sha256.New()
	bw := bundle.New(io.MultiWriter(f, hasher), manifest, r.cfg.ArtifactMaxBytes)

	addErr := func() error {
		if err := bw.AddBytes("session.json", cfgJSON); err != nil {
			return err
		}
		for _, name := range []string{"events.jsonl", "conversation.jsonl", "traces.jsonl"} {
			data, err := readFileIfExists(filepath.Join(r.cfg.ArtifactsDir, name))
			if err != nil {
				return err
			}
			if err := bw.AddBytes(name, data); err != nil {
				return err
			}
		}
		// A plugin's stderr is the only place a crashing interpreter explains
		// itself, so it travels with the session.
		for _, p := range r.cfg.Plugins {
			name := "plugins/" + p.Name + ".stderr.log"
			data, err := readFileIfExists(filepath.Join(r.cfg.ArtifactsDir, "plugins", p.Name+".stderr.log"))
			if err != nil {
				return err
			}
			if data == nil {
				continue
			}
			if err := bw.AddBytes(name, data); err != nil {
				return err
			}
		}
		return bw.AddDir("files", r.cfg.WorkspaceDir)
	}()
	if addErr != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return r.failFinalize(ctx, "build bundle: "+addErr.Error())
	}
	if err := bw.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return r.failFinalize(ctx, "close bundle: "+err.Error())
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return r.failFinalize(ctx, "sync bundle: "+err.Error())
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return r.failFinalize(ctx, "close bundle file: "+err.Error())
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return r.failFinalize(ctx, "stat bundle: "+err.Error())
	}
	if info.Size() > r.cfg.ArtifactMaxBytes {
		_ = os.Remove(tmpPath)
		return r.failFinalize(ctx, fmt.Sprintf("bundle size %d exceeds limit %d", info.Size(), r.cfg.ArtifactMaxBytes))
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return r.failFinalize(ctx, "publish bundle: "+err.Error())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))

	artifact, err := r.platform.UploadArtifact(ctx, finalPath, digest, info.Size(), runErr != nil)
	if err != nil {
		r.events.Emit(ctx, model.EventError, "mayfly", "Artifact upload failed: "+err.Error(), nil)
		_ = r.platform.Finalize(ctx, model.StatusFailed, reason, err.Error())
		return err
	}
	r.events.Emit(ctx, model.EventArtifact, "mayfly", "Artifact exported.", model.ArtifactData{
		Key:    artifact.Key,
		URI:    artifact.URI,
		Size:   artifact.Size,
		SHA256: artifact.SHA256,
	})

	finalStatus := model.StatusCompleted
	errMsg := ""
	if runErr != nil {
		finalStatus = model.StatusFailed
		errMsg = runErr.Error()
	}
	if err := r.platform.Finalize(ctx, finalStatus, reason, errMsg); err != nil {
		r.logger.Warn("finalize report failed", "error", err)
		return err
	}
	r.logger.Info("session finalized", "artifact", artifact.URI, "size", artifact.Size)
	return runErr
}

func (r *Runtime) failFinalize(ctx context.Context, message string) error {
	r.logger.Error("finalize failed", "error", message)
	r.events.Emit(ctx, model.EventError, "mayfly", "Artifact export failed: "+message, nil)
	_ = r.platform.Finalize(ctx, model.StatusFailed, "failed", message)
	return fmt.Errorf("%s", message)
}

func readFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}
