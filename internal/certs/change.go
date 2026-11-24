package certs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const generationFile = "generation"

// PublicationError reports a committed game change whose files still need copying.
type PublicationError struct{ cause error }

func (e *PublicationError) Error() string {
	return "The new game certificate mode is active. Copying certificate files to game servers did not finish and the daemon will retry automatically."
}

func (e *PublicationError) Unwrap() error { return e.cause }

func (m *Manager) stagingDir() string {
	return filepath.Join(filepath.Dir(m.dir), ".game-change")
}

// change stages outside every mount. The settings value commits the mode
// and the identity of its complete stage in one operation. After that
// point publication is independent of the client request.
func (m *Manager) change(ctx context.Context, mode Mode, stagePairs func(string, []int64) error) error {
	if _, err := m.state(ctx); err != nil {
		return err
	}
	ids, err := m.settings.ListServerIDs(ctx)
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	value := string(mode) + ":" + hex.EncodeToString(nonce[:])
	stage := m.stagingDir()
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	commitStarted := false
	defer func() {
		if !commitStarted {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := stagePairs(stage, ids); err != nil {
		return err
	}
	if err := m.writeFile(stage, generationFile, []byte(value), 0o600); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(stage)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	commitStarted = true
	commitCtx := context.WithoutCancel(ctx)
	if err := m.settings.SetSetting(commitCtx, SettingMode, value); err != nil {
		// Read the persisted decision in case the save had an ambiguous
		// result. A stage for an uncommitted value is never published.
		saved, _, readErr := m.settings.GetSetting(commitCtx, SettingMode)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		if saved != value {
			return errors.Join(err, os.RemoveAll(stage))
		}
	}
	if err := m.publish(mode); err != nil {
		m.logger.Warn("committed game certificate publication incomplete", "mode", mode, "err", err)
		select {
		case m.publicationWake <- struct{}{}:
		default:
		}
		return &PublicationError{cause: err}
	}
	return nil
}

// state recovers the committed generation before returning a mode to any
// daemon caller. An incomplete or uncommitted stage never enters a mount.
func (m *Manager) state(ctx context.Context) (Mode, error) {
	value, _, err := m.settings.GetSetting(ctx, SettingMode)
	if err != nil {
		return "", err
	}
	rawMode, generation, _ := strings.Cut(value, ":")
	mode := Mode(rawMode)
	if mode == "" {
		mode = ModeSelfSigned
	}
	if !mode.Valid() {
		return "", fmt.Errorf("certs: unknown certificate mode %q", mode)
	}
	stage := m.stagingDir()
	record, err := os.ReadFile(filepath.Join(stage, generationFile))
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	case generation != "" && string(record) == value:
		return mode, m.publish(mode)
	default:
		return mode, os.RemoveAll(stage)
	}
}

func (m *Manager) publish(mode Mode) error {
	stage := m.stagingDir()
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if mode != ModeSelfSigned {
		if err := m.copyPair(filepath.Join(stage, "custom"), m.dir); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		src := filepath.Join(stage, entry.Name())
		if mode != ModeSelfSigned {
			src = filepath.Join(stage, "custom")
		}
		if err := m.copyPair(src, m.ServerDir(id)); err != nil {
			return err
		}
	}
	if mode == ModeSelfSigned {
		if err := m.removeLegacyPair(); err != nil {
			return err
		}
	}
	// Move the entire published stage out of recovery's way before any
	// part of it is deleted, so recovery never replays a partial stage.
	done := stage + "-done"
	if err := os.RemoveAll(done); err != nil {
		return err
	}
	if err := os.Rename(stage, done); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(stage)); err != nil {
		return err
	}
	return os.RemoveAll(done)
}

func (m *Manager) removeLegacyPair() error {
	for _, name := range []string{CertFile, KeyFile, pendingCert, pendingKey} {
		if err := removeIfPresent(filepath.Join(m.dir, name)); err != nil {
			return err
		}
	}
	return syncDir(m.dir)
}

func (m *Manager) openFiles(ctx context.Context) error {
	if err := os.MkdirAll(m.serversDir(), 0o700); err != nil {
		return err
	}
	mode, err := m.state(ctx)
	if err != nil {
		return err
	}
	if err := m.recoverPending(m.dir); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(m.dir, KeyFile), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mode == ModeSelfSigned {
		if err := m.removeLegacyPair(); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(m.stagingDir() + "-done"); err != nil {
		return err
	}
	ids, err := m.settings.ListServerIDs(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[strconv.FormatInt(id, 10)] = true
	}
	entries, err := os.ReadDir(m.serversDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		dir := filepath.Join(m.serversDir(), entry.Name())
		if live[entry.Name()] {
			err = m.recoverPending(dir)
		} else {
			err = os.RemoveAll(dir)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
