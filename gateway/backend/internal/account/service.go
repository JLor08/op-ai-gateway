// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/totp"
	"strings"
	"time"
)

var (
	ErrInvalidCredentials      = errors.New("auth.invalid_credentials")
	ErrSessionInvalid          = errors.New("auth.session_invalid")
	ErrSetPasswordTokenInvalid = errors.New("auth.set_password_token_invalid")
	ErrUserConflict            = errors.New("admin.user_conflict")
	ErrUserNotFound            = errors.New("admin.user_not_found")
	ErrLastAdmin               = errors.New("admin.cannot_disable_last_admin")
	ErrLastSystemAdmin         = errors.New("admin.cannot_disable_last_system_admin")
	ErrForbiddenRole           = errors.New("admin.system_admin_forbidden")
	ErrInvalidRole             = errors.New("admin.invalid_role")
	ErrInvalidStatus           = errors.New("admin.invalid_status")
	ErrEmailRequired           = errors.New("admin.email_required")
	ErrTOTPKeyRequired         = errors.New("auth.totp_key_required")
	ErrTOTPInvalid             = errors.New("auth.totp_invalid")
	ErrTOTPNotEnrolled         = errors.New("auth.totp_not_enrolled")
	ErrNotSystemAdmin          = errors.New("account.not_system_admin")
)

type UserStore interface {
	UserByID(ctx context.Context, id string) (store.User, error)
	UserByEmail(ctx context.Context, email string) (store.User, error)
	CreateUser(ctx context.Context, user store.User) error
	UpdateUser(ctx context.Context, user store.User) error
	ListUsers(ctx context.Context) ([]store.User, error)
}

type SessionStore interface {
	CreateSession(ctx context.Context, session store.Session) error
	SessionBySecret(ctx context.Context, secretHash string) (store.Session, error)
	TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionsByUser(ctx context.Context, userID string) error
	SetSessionElevation(ctx context.Context, id string, until time.Time) error
}

type SetPasswordTokenStore interface {
	CreateSetPasswordToken(ctx context.Context, tok store.SetPasswordToken) error
	SetPasswordTokenBySecret(ctx context.Context, secretHash string) (store.SetPasswordToken, error)
	MarkSetPasswordTokenUsed(ctx context.Context, id string, usedAt time.Time) error
	InvalidateUserSetPasswordTokens(ctx context.Context, userID string) error
}

type Deps struct {
	Users             UserStore
	Sessions          SessionStore
	SetPasswordTokens SetPasswordTokenStore
	Cipher            *capture.Cipher
	SettingsVolatile  bool
}

type Config struct {
	IdleTTL            time.Duration
	MaxTTL             time.Duration
	InviteTTL          time.Duration
	SystemAdminModeTTL time.Duration
	DefaultLanguage    string
	Clock              func() time.Time
	SecretGenerator    func() (string, error)
	IDGenerator        func() string
}

type Service struct {
	users              UserStore
	sessions           SessionStore
	setPasswordTokens  SetPasswordTokenStore
	idleTTL            time.Duration
	maxTTL             time.Duration
	inviteTTL          time.Duration
	systemAdminModeTTL time.Duration
	defaultLanguage    string
	clock              func() time.Time
	secretGenerator    func() (string, error)
	idGenerator        func() string
	cipher             *capture.Cipher
	settingsVolatile   bool
}

func NewService(deps Deps, cfg Config) *Service {
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	secretGenerator := cfg.SecretGenerator
	if secretGenerator == nil {
		secretGenerator = func() (string, error) { return randomHex(32) }
	}
	idGenerator := cfg.IDGenerator
	if idGenerator == nil {
		idGenerator = func() string { id, _ := randomHex(16); return id }
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = 12 * time.Hour
	}
	maxTTL := cfg.MaxTTL
	if maxTTL <= 0 {
		maxTTL = 168 * time.Hour
	}
	inviteTTL := cfg.InviteTTL
	if inviteTTL <= 0 {
		inviteTTL = 72 * time.Hour
	}
	systemAdminModeTTL := cfg.SystemAdminModeTTL
	if systemAdminModeTTL <= 0 {
		systemAdminModeTTL = 15 * time.Minute
	}
	language := cfg.DefaultLanguage
	if language == "" {
		language = "de"
	}
	return &Service{
		users:              deps.Users,
		sessions:           deps.Sessions,
		setPasswordTokens:  deps.SetPasswordTokens,
		idleTTL:            idleTTL,
		maxTTL:             maxTTL,
		inviteTTL:          inviteTTL,
		systemAdminModeTTL: systemAdminModeTTL,
		defaultLanguage:    language,
		clock:              clock,
		secretGenerator:    secretGenerator,
		idGenerator:        idGenerator,
		cipher:             deps.Cipher,
		settingsVolatile:   deps.SettingsVolatile,
	}
}

// AuthenticatePassword verifies the given credentials and returns the user on success.
func (s *Service) AuthenticatePassword(ctx context.Context, email, password string) (store.User, error) {
	user, err := s.users.UserByEmail(ctx, email)
	if err != nil {
		return store.User{}, ErrInvalidCredentials
	}
	if user.Status != store.UserStatusActive || user.PasswordHash == "" {
		return store.User{}, ErrInvalidCredentials
	}
	if !auth.VerifyPassword(user.PasswordHash, password) {
		return store.User{}, ErrInvalidCredentials
	}
	return user, nil
}

// IssueSession creates a session for the given user and returns the session secret.
func (s *Service) IssueSession(ctx context.Context, user store.User) (string, error) {
	secret, err := s.secretGenerator()
	if err != nil {
		return "", err
	}
	now := s.clock().UTC()
	session := store.Session{
		ID:         "sess_" + s.idGenerator(),
		UserID:     user.ID,
		SecretHash: auth.HashSecret(secret),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.maxTTL),
		LastSeenAt: now,
	}
	if err := s.sessions.CreateSession(ctx, session); err != nil {
		return "", err
	}
	return secret, nil
}

// Login verifies credentials and creates a session, returning the session secret.
func (s *Service) Login(ctx context.Context, email string, password string) (store.User, string, error) {
	user, err := s.AuthenticatePassword(ctx, email, password)
	if err != nil {
		return store.User{}, "", err
	}
	secret, err := s.IssueSession(ctx, user)
	if err != nil {
		return store.User{}, "", err
	}
	return user, secret, nil
}

// ResolveSessionDetail validates a session secret and returns the active user
// AND the session record (needed for the session's ElevatedUntil).
func (s *Service) ResolveSessionDetail(ctx context.Context, secret string) (store.User, store.Session, error) {
	if secret == "" {
		return store.User{}, store.Session{}, ErrSessionInvalid
	}
	session, err := s.sessions.SessionBySecret(ctx, auth.HashSecret(secret))
	if err != nil {
		return store.User{}, store.Session{}, ErrSessionInvalid
	}
	now := s.clock().UTC()
	if !session.ExpiresAt.After(now) || now.Sub(session.LastSeenAt) > s.idleTTL {
		_ = s.sessions.DeleteSession(ctx, session.ID)
		return store.User{}, store.Session{}, ErrSessionInvalid
	}
	user, err := s.users.UserByID(ctx, session.UserID)
	if err != nil || user.Status != store.UserStatusActive {
		_ = s.sessions.DeleteSession(ctx, session.ID)
		return store.User{}, store.Session{}, ErrSessionInvalid
	}
	if err := s.sessions.TouchSession(ctx, session.ID, now); err != nil {
		return store.User{}, store.Session{}, ErrSessionInvalid
	}
	return user, session, nil
}

// ResolveSession validates a session secret and returns the active user.
func (s *Service) ResolveSession(ctx context.Context, secret string) (store.User, error) {
	user, _, err := s.ResolveSessionDetail(ctx, secret)
	return user, err
}

// EnterSystemAdminMode elevates the session identified by secret into
// System-Admin mode. The session's user must be a system_admin. When
// requirePassword is true the account password is verified. On success the
// session's elevated_until is set to now + the configured TTL.
func (s *Service) EnterSystemAdminMode(ctx context.Context, secret, password string, requirePassword bool) error {
	user, session, err := s.ResolveSessionDetail(ctx, secret)
	if err != nil {
		return err
	}
	if user.Role != "system_admin" {
		return ErrNotSystemAdmin
	}
	if requirePassword {
		if _, err := s.AuthenticatePassword(ctx, user.Email, password); err != nil {
			return ErrInvalidCredentials
		}
	}
	until := s.clock().UTC().Add(s.systemAdminModeTTL)
	return s.sessions.SetSessionElevation(ctx, session.ID, until)
}

// ExitSystemAdminMode drops the session's elevation. Idempotent.
func (s *Service) ExitSystemAdminMode(ctx context.Context, secret string) error {
	_, session, err := s.ResolveSessionDetail(ctx, secret)
	if err != nil {
		return err
	}
	return s.sessions.SetSessionElevation(ctx, session.ID, time.Time{})
}

// Logout revokes the session identified by the given secret. It is idempotent.
func (s *Service) Logout(ctx context.Context, secret string) error {
	if secret == "" {
		return nil
	}
	session, err := s.sessions.SessionBySecret(ctx, auth.HashSecret(secret))
	if err != nil {
		return nil
	}
	return s.sessions.DeleteSession(ctx, session.ID)
}

func randomHex(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// issueSetPasswordToken creates a fresh single-use set-password token and returns its secret.
func (s *Service) issueSetPasswordToken(ctx context.Context, userID string, now time.Time) (string, error) {
	secret, err := s.secretGenerator()
	if err != nil {
		return "", err
	}
	tok := store.SetPasswordToken{
		ID:         "spt_" + s.idGenerator(),
		UserID:     userID,
		SecretHash: auth.HashSecret(secret),
		ExpiresAt:  now.Add(s.inviteTTL),
		CreatedAt:  now,
	}
	if err := s.setPasswordTokens.CreateSetPasswordToken(ctx, tok); err != nil {
		return "", err
	}
	return secret, nil
}

// SetPassword redeems a set-password token, sets the password, activates the user, and creates a session.
func (s *Service) SetPassword(ctx context.Context, tokenSecret string, newPassword string) (store.User, string, error) {
	if err := auth.ValidatePasswordPolicy(newPassword); err != nil {
		return store.User{}, "", err
	}
	tok, err := s.setPasswordTokens.SetPasswordTokenBySecret(ctx, auth.HashSecret(tokenSecret))
	if err != nil {
		return store.User{}, "", ErrSetPasswordTokenInvalid
	}
	now := s.clock().UTC()
	if tok.UsedAt != nil || !tok.ExpiresAt.After(now) {
		return store.User{}, "", ErrSetPasswordTokenInvalid
	}
	user, err := s.users.UserByID(ctx, tok.UserID)
	if err != nil {
		return store.User{}, "", ErrSetPasswordTokenInvalid
	}
	if user.Status == store.UserStatusDisabled {
		return store.User{}, "", ErrSetPasswordTokenInvalid
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return store.User{}, "", err
	}
	user.PasswordHash = hash
	user.PasswordSetAt = &now
	user.Status = store.UserStatusActive
	user.UpdatedAt = now
	if err := s.users.UpdateUser(ctx, user); err != nil {
		return store.User{}, "", err
	}
	if err := s.setPasswordTokens.MarkSetPasswordTokenUsed(ctx, tok.ID, now); err != nil {
		return store.User{}, "", err
	}
	secret, err := s.secretGenerator()
	if err != nil {
		return store.User{}, "", err
	}
	session := store.Session{
		ID:         "sess_" + s.idGenerator(),
		UserID:     user.ID,
		SecretHash: auth.HashSecret(secret),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.maxTTL),
		LastSeenAt: now,
	}
	if err := s.sessions.CreateSession(ctx, session); err != nil {
		return store.User{}, "", err
	}
	return user, secret, nil
}

// ChangePassword verifies the current password and stores a new one.
func (s *Service) ChangePassword(ctx context.Context, userID string, currentPassword string, newPassword string) error {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return ErrInvalidCredentials
	}
	if !auth.VerifyPassword(user.PasswordHash, currentPassword) {
		return ErrInvalidCredentials
	}
	if err := auth.ValidatePasswordPolicy(newPassword); err != nil {
		return err
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	user.PasswordHash = hash
	user.PasswordSetAt = &now
	user.UpdatedAt = now
	return s.users.UpdateUser(ctx, user)
}

// VerifyTOTP checks a TOTP code against the user's stored secret. It returns
// false (never an error) if the secret cannot be opened, since a caller only
// cares whether the code is valid.
func (s *Service) VerifyTOTP(u store.User, code string) bool {
	if u.TOTPSecret == "" {
		return false
	}
	secret, err := s.openSecret(u.TOTPSecret)
	if err != nil {
		return false
	}
	return totp.Verify(secret, code, time.Now())
}

// UserByID returns the user by id. It exists so callers that only hold an
// *account.Service (e.g. gateway handlers, which have no direct store handle)
// can look up a user's current TOTP enrollment state -- for example to
// require a step-up code before allowing re-enrollment (see handleTOTPEnroll).
func (s *Service) UserByID(ctx context.Context, id string) (store.User, error) {
	user, err := s.users.UserByID(ctx, id)
	if err != nil {
		return store.User{}, ErrUserNotFound
	}
	return user, nil
}

// SetPendingTOTP generates a new TOTP secret for the user, seals it, and
// stores it into a SEPARATE pending slot (TOTPPendingSecret) -- it never
// touches the user's existing confirmed TOTPSecret/TOTPEnabled. This matters
// for re-enrollment: without a proof-of-possession step-up check (see
// handleTOTPEnroll), simply calling SetPendingTOTP must not disable or
// rebind the live factor, or a hijacked session cookie (without the physical
// authenticator) could downgrade or take over 2FA. The live secret keeps
// authenticating logins until ConfirmTOTP verifies the NEW secret and
// promotes it. Returns the base32 secret plus its otpauth:// URI for
// enrollment (e.g. rendering a QR code).
func (s *Service) SetPendingTOTP(ctx context.Context, userID string) (string, string, error) {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return "", "", ErrUserNotFound
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		return "", "", err
	}
	sealed, err := s.sealSecret(secret)
	if err != nil {
		return "", "", err
	}
	user.TOTPPendingSecret = sealed
	user.UpdatedAt = s.clock().UTC()
	if err := s.users.UpdateUser(ctx, user); err != nil {
		return "", "", err
	}
	return secret, totp.OtpauthURI("OP AI Gateway", user.Email, secret), nil
}

// ConfirmTOTP verifies the given code against the user's PENDING secret
// (never the live one -- confirming must prove possession of the NEW
// factor) and, on success, promotes it: the pending secret becomes the live
// TOTPSecret, TOTP is marked enabled, and the pending slot is cleared. This
// is the only path that can rebind an already-enrolled account, and it
// requires a valid code from the new authenticator.
func (s *Service) ConfirmTOTP(ctx context.Context, userID, code string) (store.User, error) {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return store.User{}, ErrUserNotFound
	}
	if user.TOTPPendingSecret == "" {
		return store.User{}, ErrTOTPNotEnrolled
	}
	pending, err := s.openSecret(user.TOTPPendingSecret)
	if err != nil {
		return store.User{}, err
	}
	if !totp.Verify(pending, code, time.Now()) {
		return store.User{}, ErrTOTPInvalid
	}
	now := s.clock().UTC()
	user.TOTPSecret = user.TOTPPendingSecret
	user.TOTPPendingSecret = ""
	user.TOTPEnabled = true
	user.TOTPConfirmedAt = &now
	user.UpdatedAt = now
	if err := s.users.UpdateUser(ctx, user); err != nil {
		return store.User{}, err
	}
	return user, nil
}

// DisableTOTP verifies the given code and, on success, clears the user's
// TOTP enrollment entirely.
func (s *Service) DisableTOTP(ctx context.Context, userID, code string) error {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return ErrUserNotFound
	}
	if !s.VerifyTOTP(user, code) {
		return ErrTOTPInvalid
	}
	return s.clearTOTP(ctx, user)
}

// ResetTOTP is the admin-initiated equivalent of DisableTOTP: it clears the
// user's TOTP enrollment without a code and revokes all their sessions,
// mirroring the disable-user session revoke in UpdateUser.
func (s *Service) ResetTOTP(ctx context.Context, userID string) error {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return ErrUserNotFound
	}
	if err := s.clearTOTP(ctx, user); err != nil {
		return err
	}
	return s.sessions.DeleteSessionsByUser(ctx, userID)
}

func (s *Service) clearTOTP(ctx context.Context, user store.User) error {
	user.TOTPSecret = ""
	user.TOTPPendingSecret = ""
	user.TOTPEnabled = false
	user.TOTPConfirmedAt = nil
	user.UpdatedAt = s.clock().UTC()
	return s.users.UpdateUser(ctx, user)
}

type InviteInput struct {
	Email             string
	DisplayName       string
	Role              string
	PreferredLanguage string
}

type UserUpdate struct {
	DisplayName          *string
	Role                 *string
	Status               *string
	PreferredLanguage    *string
	ChatLogCommunication *bool
	ChatSecret           *bool
}

func validRole(role string) bool {
	return role == "admin" || role == "user" || role == "system_admin"
}
func isAdminCapable(role string) bool { return role == "admin" || role == "system_admin" }
func isSystemAdmin(role string) bool  { return role == "system_admin" }
func validStatus(status string) bool {
	return status == store.UserStatusActive || status == store.UserStatusDisabled || status == store.UserStatusInvited
}

// InviteUser creates an invited user and returns a one-time set-password secret.
func (s *Service) InviteUser(ctx context.Context, in InviteInput, actorIsSystemAdmin bool) (store.User, string, error) {
	email := normalizeEmail(in.Email)
	if email == "" {
		return store.User{}, "", ErrEmailRequired
	}
	role := in.Role
	if role == "" {
		role = "user"
	}
	if !validRole(role) {
		return store.User{}, "", ErrInvalidRole
	}
	if isSystemAdmin(role) && !actorIsSystemAdmin {
		return store.User{}, "", ErrForbiddenRole
	}
	language := in.PreferredLanguage
	if language == "" {
		language = s.defaultLanguage
	}
	displayName := in.DisplayName
	if displayName == "" {
		displayName = email
	}
	now := s.clock().UTC()
	user := store.User{
		ID:                "usr_" + s.idGenerator(),
		Email:             email,
		DisplayName:       displayName,
		Role:              role,
		Status:            store.UserStatusInvited,
		PreferredLanguage: language,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.users.CreateUser(ctx, user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, "", ErrUserConflict
		}
		return store.User{}, "", err
	}
	secret, err := s.issueSetPasswordToken(ctx, user.ID, now)
	if err != nil {
		return store.User{}, "", err
	}
	return user, secret, nil
}

// ReissueInvite invalidates prior invites and issues a fresh set-password secret.
func (s *Service) ReissueInvite(ctx context.Context, userID string) (store.User, string, error) {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return store.User{}, "", ErrUserNotFound
	}
	if err := s.setPasswordTokens.InvalidateUserSetPasswordTokens(ctx, userID); err != nil {
		return store.User{}, "", err
	}
	secret, err := s.issueSetPasswordToken(ctx, userID, s.clock().UTC())
	if err != nil {
		return store.User{}, "", err
	}
	return user, secret, nil
}

func (s *Service) ListUsers(ctx context.Context) ([]store.User, error) {
	return s.users.ListUsers(ctx)
}

// UpdateUser applies admin changes, guarding against removing the last active admin
// and revoking sessions when a user is disabled.
func (s *Service) UpdateUser(ctx context.Context, userID string, update UserUpdate, actorIsSystemAdmin bool) (store.User, error) {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return store.User{}, ErrUserNotFound
	}
	if update.Role != nil && !validRole(*update.Role) {
		return store.User{}, ErrInvalidRole
	}
	targetIsSystemAdmin := update.Role != nil && *update.Role == "system_admin"
	if (isSystemAdmin(user.Role) || targetIsSystemAdmin) && !actorIsSystemAdmin {
		return store.User{}, ErrForbiddenRole
	}
	wasActiveAdminCapable := user.Status == store.UserStatusActive && isAdminCapable(user.Role)
	wasActiveSystemAdmin := user.Status == store.UserStatusActive && isSystemAdmin(user.Role)

	if update.DisplayName != nil {
		user.DisplayName = *update.DisplayName
	}
	if update.PreferredLanguage != nil && *update.PreferredLanguage != "" {
		user.PreferredLanguage = *update.PreferredLanguage
	}
	if update.ChatLogCommunication != nil {
		user.ChatLogCommunication = *update.ChatLogCommunication
	}
	if update.ChatSecret != nil {
		user.ChatSecret = *update.ChatSecret
	}
	if update.Role != nil {
		user.Role = *update.Role
	}
	if update.Status != nil {
		if !validStatus(*update.Status) {
			return store.User{}, ErrInvalidStatus
		}
		user.Status = *update.Status
	}

	stillActiveAdminCapable := user.Status == store.UserStatusActive && isAdminCapable(user.Role)
	if wasActiveAdminCapable && !stillActiveAdminCapable {
		remaining, err := s.otherActiveAdmins(ctx, user.ID)
		if err != nil {
			return store.User{}, err
		}
		if remaining == 0 {
			return store.User{}, ErrLastAdmin
		}
	}
	stillActiveSystemAdmin := user.Status == store.UserStatusActive && isSystemAdmin(user.Role)
	if wasActiveSystemAdmin && !stillActiveSystemAdmin {
		remaining, err := s.otherActiveSystemAdmins(ctx, user.ID)
		if err != nil {
			return store.User{}, err
		}
		if remaining == 0 {
			return store.User{}, ErrLastSystemAdmin
		}
	}

	user.UpdatedAt = s.clock().UTC()
	if err := s.users.UpdateUser(ctx, user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, ErrUserConflict
		}
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrUserNotFound
		}
		return store.User{}, err
	}
	if user.Status == store.UserStatusDisabled {
		if err := s.sessions.DeleteSessionsByUser(ctx, user.ID); err != nil {
			return store.User{}, err
		}
		if err := s.setPasswordTokens.InvalidateUserSetPasswordTokens(ctx, user.ID); err != nil {
			return store.User{}, err
		}
	}
	return user, nil
}

// UpdateOwnProfile applies a self-service edit of a user's own non-role
// preferences (preferred language + chat capture settings). It never changes
// Role or Status, so the system-admin-account protection guard in UpdateUser
// does NOT apply — a user editing their own non-role fields never needs
// system-admin elevation, even when their own role is system_admin.
func (s *Service) UpdateOwnProfile(ctx context.Context, userID string, update UserUpdate) (store.User, error) {
	user, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return store.User{}, ErrUserNotFound
	}
	if update.PreferredLanguage != nil && *update.PreferredLanguage != "" {
		user.PreferredLanguage = *update.PreferredLanguage
	}
	if update.ChatLogCommunication != nil {
		user.ChatLogCommunication = *update.ChatLogCommunication
	}
	if update.ChatSecret != nil {
		user.ChatSecret = *update.ChatSecret
	}
	user.UpdatedAt = s.clock().UTC()
	if err := s.users.UpdateUser(ctx, user); err != nil {
		return store.User{}, err
	}
	return user, nil
}

func (s *Service) otherActiveAdmins(ctx context.Context, excludeID string) (int, error) {
	users, err := s.users.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, u := range users {
		if u.ID == excludeID {
			continue
		}
		if u.Status == store.UserStatusActive && isAdminCapable(u.Role) {
			count++
		}
	}
	return count, nil
}

func (s *Service) otherActiveSystemAdmins(ctx context.Context, excludeID string) (int, error) {
	users, err := s.users.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, u := range users {
		if u.ID == excludeID {
			continue
		}
		if u.Status == store.UserStatusActive && isSystemAdmin(u.Role) {
			count++
		}
	}
	return count, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// plainPrefix marks a volatile, unsealed TOTP secret value (see
// sealSecret/openSecret).
const plainPrefix = "plain:"

// sealSecret encodes a TOTP secret for storage. With a cipher it seals to
// "enc:"+base64; on the volatile in-memory store (no cipher) it stores
// "plain:"+raw (never written to disk, gone on process exit — same rationale as
// the RAM capture fallback); on a disk store without a cipher it refuses with
// ErrTOTPKeyRequired rather than persist plaintext.
func (s *Service) sealSecret(plain string) (string, error) {
	if s.cipher != nil {
		return "enc:" + base64.StdEncoding.EncodeToString(s.cipher.Seal([]byte(plain))), nil
	}
	if s.settingsVolatile {
		return plainPrefix + plain, nil
	}
	return "", ErrTOTPKeyRequired
}

// openSecret reverses sealSecret. An empty value (no secret stored) opens to
// "". An "enc:" value requires the cipher (ErrTOTPKeyRequired if the key was
// removed after sealing); a "plain:" value returns the raw secret. Any other
// shape is treated as missing key rather than leaking a corrupt value.
func (s *Service) openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if strings.HasPrefix(stored, plainPrefix) {
		return strings.TrimPrefix(stored, plainPrefix), nil
	}
	if strings.HasPrefix(stored, "enc:") {
		if s.cipher == nil {
			return "", ErrTOTPKeyRequired
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
		if err != nil {
			return "", err
		}
		plain, err := s.cipher.Open(raw)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	return "", ErrTOTPKeyRequired
}
