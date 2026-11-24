package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Setting keys. Values are stored as text.
const (
	SettingRegistrationEnabled = "registration_enabled"
	// SettingGameHost is the DNS name or IP address players use to reach
	// game servers ("" falls back to the first --public-host).
	SettingGameHost = "game_host"
	// SettingGameCertificateMode is the game certificate mode
	// (internal/certs; "" means the default, self-signed).
	SettingGameCertificateMode = "game_certificate_mode"
)

// Settings is the typed view of the settings table. Missing keys take the
// zero value, which must always be the safe default.
type Settings struct {
	RegistrationEnabled bool
	// GameHost is the validated game host ("" when not set).
	GameHost string
}

// SettingsUpdate changes settings. Nil fields are unchanged. Callers
// validate values first.
type SettingsUpdate struct {
	RegistrationEnabled *bool
	GameHost            *string
}

// GetSetting returns a raw setting value and whether it exists.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, mapErr("get setting", err)
	}
	return v, true, nil
}

// SetSetting upserts a raw setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return mapErr("set setting", err)
}

// GetSettings returns all typed settings.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var out Settings
	v, ok, err := s.GetSetting(ctx, SettingRegistrationEnabled)
	if err != nil {
		return Settings{}, err
	}
	if ok {
		out.RegistrationEnabled, _ = strconv.ParseBool(v)
	}
	if out.GameHost, _, err = s.GetSetting(ctx, SettingGameHost); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// UpdateSettings applies upd and returns the resulting settings.
func (s *Store) UpdateSettings(ctx context.Context, upd SettingsUpdate) (Settings, error) {
	if upd.RegistrationEnabled != nil {
		if err := s.SetSetting(ctx, SettingRegistrationEnabled, strconv.FormatBool(*upd.RegistrationEnabled)); err != nil {
			return Settings{}, err
		}
	}
	if upd.GameHost != nil {
		if err := s.SetSetting(ctx, SettingGameHost, *upd.GameHost); err != nil {
			return Settings{}, err
		}
	}
	return s.GetSettings(ctx)
}
