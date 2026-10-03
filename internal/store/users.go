package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// userColumns is the column list every user read shares, in the order scanUser
// expects. Kept in one place so a new column cannot be added to one query and
// forgotten in another. Every name is qualified because the session lookup joins
// sessions, which has an id and a created_at of its own.
const userColumns = `SELECT u.id, u.host, u.remote_id, u.acct, u.display_name, u.avatar_url,
	u.access_token, u.timezone, u.default_template, u.created_at, u.last_login_at
	FROM users u`

// UpsertUser creates or refreshes a user identified by (host, remote_id).
func (s *Store) UpsertUser(ctx context.Context, u *User) (*User, error) {
	now := time.Now().UTC()
	var created int64
	err := s.rw.QueryRowContext(ctx, `
		INSERT INTO users (host, remote_id, acct, display_name, avatar_url, access_token, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host, remote_id) DO UPDATE SET
			acct = excluded.acct,
			display_name = excluded.display_name,
			avatar_url = excluded.avatar_url,
			access_token = excluded.access_token,
			last_login_at = excluded.last_login_at
		RETURNING id, created_at, timezone, default_template`,
		u.Host, u.RemoteID, u.Acct, u.DisplayName, u.AvatarURL, u.AccessToken, now.Unix(), now.Unix(),
	).Scan(&u.ID, &created, &u.Timezone, &u.DefaultTemplate)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.LastLoginAt = now
	return u, nil
}

// UserByRemote finds a user by the identity an instance reports.
func (s *Store) UserByRemote(ctx context.Context, host, remoteID string) (*User, error) {
	return s.scanUser(s.ro.QueryRowContext(ctx,
		userColumns+` WHERE u.host = ? AND u.remote_id = ?`, host, remoteID))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.scanUser(s.ro.QueryRowContext(ctx, userColumns+` WHERE u.id = ?`, id))
}

// SetUserPreferences stores the account-level settings. Both values are
// validated by the caller; empty means "the default", not "unset".
func (s *Store) SetUserPreferences(ctx context.Context, id int64, timezone, defaultTemplate string) error {
	res, err := s.rw.ExecContext(ctx,
		`UPDATE users SET timezone = ?, default_template = ? WHERE id = ?`,
		timezone, defaultTemplate, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SwapUserToken replaces the sealed access token an account signed in with and
// returns the one it replaced, read in the same transaction so it is the token
// that was actually on record rather than whatever a cached copy of the account
// remembered. Used when the connect flow authorises the signed-in account
// itself: the new token then has to be the one every path treats as the
// account's own, and the previous one is the caller's to revoke.
func (s *Store) SwapUserToken(ctx context.Context, id int64, sealed []byte) ([]byte, error) {
	var previous []byte
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT access_token FROM users WHERE id = ?`, id).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET access_token = ? WHERE id = ?`, sealed, id)
		return err
	})
	return previous, err
}

// UserTimezone reads one account's timezone without pulling its access token
// along with it. The delivery worker needs the zone for every post and nothing
// else from the row.
func (s *Store) UserTimezone(ctx context.Context, id int64) (string, error) {
	var tz string
	err := s.ro.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id = ?`, id).Scan(&tz)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tz, err
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var created, login int64
	err := row.Scan(&u.ID, &u.Host, &u.RemoteID, &u.Acct, &u.DisplayName, &u.AvatarURL, &u.AccessToken,
		&u.Timezone, &u.DefaultTemplate, &created, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.LastLoginAt = time.Unix(login, 0).UTC()
	return &u, nil
}

// DeleteUser removes the account and, by cascade, everything owned by it.
//
// The subscription goes with the account, but the feed it pointed at does not:
// it is shared, and somebody else may still be reading it. Collecting the ones
// nobody is left reading has to happen here rather than being left to the
// janitor, or a deleted account's feed goes on being fetched for up to an hour
// with no one to deliver it to.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
			return err
		}
		return deleteOrphanFeeds(ctx, tx)
	})
}

// deleteOrphanFeeds removes feeds nobody subscribes to, taking their entries
// and polling state with them.
func deleteOrphanFeeds(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM feeds
		WHERE NOT EXISTS (SELECT 1 FROM subscriptions sub WHERE sub.feed_id = feeds.id)`)
	return err
}

// DeleteOrphanFeeds is the janitor's backstop for feeds left without a
// subscriber. Every path that drops a subscription already collects behind
// itself; this catches whatever a crash left half-done.
func (s *Store) DeleteOrphanFeeds(ctx context.Context) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM feeds
			WHERE NOT EXISTS (SELECT 1 FROM subscriptions sub WHERE sub.feed_id = feeds.id)`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// CountRecentUsersByHost reports how many accounts a single instance has
// registered since a given time.
func (s *Store) CountRecentUsersByHost(ctx context.Context, host string, since time.Time) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE host = ? AND created_at >= ?`, host, since.Unix()).Scan(&n)
	return n, err
}

// CountUsers reports the number of registered accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// --- OAuth client registrations -------------------------------------------

func (s *Store) Instance(ctx context.Context, host string) (*Instance, error) {
	var in Instance
	var created int64
	err := s.ro.QueryRowContext(ctx,
		`SELECT host, client_id, client_secret, redirect_uri, created_at FROM instances WHERE host = ?`, host,
	).Scan(&in.Host, &in.ClientID, &in.ClientSecret, &in.RedirectURI, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	in.CreatedAt = time.Unix(created, 0).UTC()
	return &in, nil
}

func (s *Store) SaveInstance(ctx context.Context, in *Instance) error {
	_, err := s.rw.ExecContext(ctx, `
		INSERT INTO instances (host, client_id, client_secret, redirect_uri, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET
			client_id = excluded.client_id,
			client_secret = excluded.client_secret,
			redirect_uri = excluded.redirect_uri,
			created_at = excluded.created_at`,
		in.Host, in.ClientID, in.ClientSecret, in.RedirectURI, time.Now().UTC().Unix())
	return err
}

// --- OAuth states ----------------------------------------------------------

// What an OAuth flow is for. Recorded when the flow starts, so the callback
// never has to be told by a parameter.
const (
	// OAuthLogin signs an account in, creating it if need be.
	OAuthLogin = "login"
	// OAuthConnect adds a further Mastodon account as a destination of the
	// account that started the flow.
	OAuthConnect = "connect"
)

// OAuthState is what is remembered about an authorisation in flight: the
// instance it was started against, what it is for, and — for a connect — whose
// destination the result becomes.
type OAuthState struct {
	Host    string
	Purpose string
	// UserID is the account that started a connect flow, and zero for a login.
	UserID int64
}

// PutOAuthState records an in-flight authorisation. id is the hash of the state
// value handed to the browser.
func (s *Store) PutOAuthState(ctx context.Context, id string, st OAuthState, verifier []byte, ttl time.Duration) error {
	now := time.Now().UTC()
	var userID any
	if st.UserID != 0 {
		userID = st.UserID
	}
	if st.Purpose == "" {
		st.Purpose = OAuthLogin
	}
	_, err := s.rw.ExecContext(ctx, `
		INSERT INTO oauth_states (id, host, purpose, user_id, code_verifier, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, st.Host, st.Purpose, userID, verifier, now.Unix(), now.Add(ttl).Unix())
	return err
}

// TakeOAuthState consumes a state exactly once, returning what was recorded
// when the flow started. A replayed callback finds nothing.
func (s *Store) TakeOAuthState(ctx context.Context, id string) (OAuthState, []byte, error) {
	var st OAuthState
	var verifier []byte
	var userID sql.NullInt64
	var expires int64
	err := s.rw.QueryRowContext(ctx,
		`DELETE FROM oauth_states WHERE id = ? RETURNING host, purpose, user_id, code_verifier, expires_at`, id,
	).Scan(&st.Host, &st.Purpose, &userID, &verifier, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthState{}, nil, ErrNotFound
	}
	if err != nil {
		return OAuthState{}, nil, err
	}
	if time.Now().UTC().Unix() > expires {
		return OAuthState{}, nil, ErrNotFound
	}
	st.UserID = userID.Int64
	return st, verifier, nil
}

// --- Sessions --------------------------------------------------------------

func (s *Store) CreateSession(ctx context.Context, id string, userID int64, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := s.rw.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, userID, now.Unix(), now.Add(ttl).Unix())
	return err
}

// SessionUser returns the user for a live session, or ErrNotFound if the
// session is unknown or expired.
func (s *Store) SessionUser(ctx context.Context, sessionID string) (*User, error) {
	return s.scanUser(s.ro.QueryRowContext(ctx,
		userColumns+` JOIN sessions s ON s.user_id = u.id
		WHERE s.id = ? AND s.expires_at > ?`, sessionID, time.Now().UTC().Unix()))
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.rw.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

// DeleteExpired clears rows that have outlived their usefulness.
func (s *Store) DeleteExpired(ctx context.Context) error {
	now := time.Now().UTC().Unix()
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now); err != nil {
		return err
	}
	_, err := s.rw.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < ?`, now)
	return err
}
