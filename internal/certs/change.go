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
	if err := m.finishPublication(ctx); err != nil {
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
			m.wakePublisher()
			return errors.Join(err, readErr)
		}
		if saved != value {
			return errors.Join(err, os.RemoveAll(stage))
		}
	}
	if err := m.publish(commitCtx, mode); err != nil {
		m.logger.Warn("committed game certificate publication incomplete", "mode", mode, "err", err)
		m.wakePublisher()
		return &PublicationError{cause: err}
	}
	return nil
}

func (m *Manager) wakePublisher() {
	select {
	case m.publicationWake <- struct{}{}:
	default:
	}
}

func (m *Manager) committedMode(ctx context.Context) (Mode, string, error) {
	value, _, err := m.settings.GetSetting(ctx, SettingMode)
	if err != nil {
		return "", "", err
	}
	rawMode, generation, _ := strings.Cut(value, ":")
	mode := Mode(rawMode)
	if mode == "" {
		mode = ModeSelfSigned
	}
	if !mode.Valid() {
		return "", "", fmt.Errorf("certs: unknown certificate mode %q", mode)
	}
	if generation == "" {
		value = ""
	}
	return mode, value, nil
}

func (m *Manager) committedStage(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	stage := m.stagingDir()
	record, err := os.ReadFile(filepath.Join(stage, generationFile))
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	case string(record) == value:
		return stage, nil
	default:
		return "", nil
	}
}

func (m *Manager) finishPublication(ctx context.Context) error {
	mode, value, err := m.committedMode(ctx)
	if err != nil {
		return err
	}
	stage, err := m.committedStage(value)
	if err != nil {
		return err
	}
	if stage == "" {
		return os.RemoveAll(m.stagingDir())
	}
	return m.publish(ctx, mode)
}

func (m *Manager) publish(ctx context.Context, mode Mode) error {
	stage := m.stagingDir()
	ids, err := m.settings.ListServerIDs(ctx)
	if err != nil {
		return err
	}
	if mode != ModeSelfSigned {
		if err := m.copyPair(filepath.Join(stage, "custom"), m.dir); err != nil {
			return err
		}
	}
	var failures error
	held := false
	for _, id := range ids {
		if _, err := m.publishServer(stage, id, mode); errors.Is(err, errMountHeld) {
			held = true
		} else if err != nil {
			failures = errors.Join(failures, err)
		}
	}
	if failures != nil {
		return failures
	}
	if held {
		return errMountHeld
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

func (m *Manager) publishServer(stage string, id int64, mode Mode) (bool, error) {
	if stage == "" {
		return false, nil
	}
	entry := filepath.Join(stage, strconv.FormatInt(id, 10))
	if _, err := os.Stat(entry); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	src := entry
	if mode != ModeSelfSigned {
		src = filepath.Join(stage, "custom")
	}
	if err := m.copyPair(src, m.ServerDir(id)); err != nil {
		return true, err
	}
	// Retire the entry only after the mount is durable. A later start or
	// self-signed rotation can then change the mount without a stage replay.
	if err := os.Rename(entry, entry+"-done"); err != nil {
		return true, err
	}
	return true, syncDir(stage)
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
	mode, _, err := m.committedMode(ctx)
	if err != nil {
		return err
	}
	if err := m.finishPublication(ctx); err != nil {
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
