package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AuthEventKind values, per design §3.2. These exist to measure the real
// re-auth cadence, which nobody knows in advance.
type AuthEventKind string

const (
	AuthProbeOK         AuthEventKind = "probe_ok"
	AuthExpired         AuthEventKind = "expired"
	AuthExpiringWarning AuthEventKind = "expiring_warning"
	AuthLoginStarted    AuthEventKind = "login_started"
	AuthLoginCompleted  AuthEventKind = "login_completed"
	AuthLoginFailed     AuthEventKind = "login_failed"
)

type AuthEvent struct {
	ID     int64         `json:"id"`
	At     time.Time     `json:"at"`
	Kind   AuthEventKind `json:"kind"`
	Detail string        `json:"detail,omitempty"`
}

func (s *Store) RecordAuthEvent(ctx context.Context, kind AuthEventKind, detail string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_events (at, kind, detail) VALUES (?,?,?)`,
		toMillis(now), string(kind), nullString(detail))
	if err != nil {
		return fmt.Errorf("record auth event %s: %w", kind, err)
	}
	return nil
}

// LastAuthEvent returns the most recent event of a kind.
func (s *Store) LastAuthEvent(ctx context.Context, kind AuthEventKind) (AuthEvent, bool, error) {
	var (
		e      AuthEvent
		at     int64
		detail sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, at, kind, detail FROM auth_events WHERE kind = ? ORDER BY at DESC, id DESC LIMIT 1`,
		string(kind)).Scan(&e.ID, &at, &e.Kind, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("read last auth event %s: %w", kind, err)
	}
	e.At = fromMillis(at)
	e.Detail = detail.String
	return e, true, nil
}

func (s *Store) AuthEvents(ctx context.Context, limit int) ([]AuthEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, kind, detail FROM auth_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list auth events: %w", err)
	}
	defer rows.Close()
	var out []AuthEvent
	for rows.Next() {
		var (
			e      AuthEvent
			at     int64
			detail sql.NullString
		)
		if err := rows.Scan(&e.ID, &at, &e.Kind, &detail); err != nil {
			return nil, err
		}
		e.At = fromMillis(at)
		e.Detail = detail.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuthEventCount counts events of a kind, which is where
// spool_auth_relogins_total comes from.
func (s *Store) AuthEventCount(ctx context.Context, kind AuthEventKind) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_events WHERE kind = ?`, string(kind)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count auth events %s: %w", kind, err)
	}
	return n, nil
}

// SessionLifetimes pairs each completed login with the next expiry, giving the
// observed lifetime of each session. This is the measurement the credential-mode
// decision rests on (§3.2, §8).
//
// A login with no subsequent expiry is still alive and is not reported.
func (s *Store) SessionLifetimes(ctx context.Context) ([]time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, kind FROM auth_events
		WHERE kind IN (?, ?) ORDER BY at, id`,
		string(AuthLoginCompleted), string(AuthExpired))
	if err != nil {
		return nil, fmt.Errorf("read session lifetimes: %w", err)
	}
	defer rows.Close()

	var out []time.Duration
	var loginAt *time.Time
	for rows.Next() {
		var at int64
		var kind string
		if err := rows.Scan(&at, &kind); err != nil {
			return nil, err
		}
		t := fromMillis(at)
		switch AuthEventKind(kind) {
		case AuthLoginCompleted:
			// A second login with no expiry between them means the first session
			// ended unobserved; keep the later one.
			loginAt = &t
		case AuthExpired:
			if loginAt != nil {
				out = append(out, t.Sub(*loginAt))
				loginAt = nil
			}
		}
	}
	return out, rows.Err()
}
