package sqlite

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/scrypt"

	"cdamp/internal/domain"
)

// Encryption at rest for signing_keys.private_key_encrypted
// -----------------------------------------------------------------------
// Per the cdamp-signing and cdamp-sqlite-fts skills' "Encryption at rest"
// sections, the private key is encrypted under a passphrase-derived key
// (scrypt KDF over the passphrase + a random per-row salt) with a standard
// AEAD (AES-256-GCM via stdlib crypto/cipher) — no bespoke cipher
// construction. 03-API.md's schema has exactly one BLOB column for this
// (no separate salt/nonce columns), so the encrypted blob must be
// self-describing. Layout (all fixed-width except the trailing
// ciphertext, which runs to the end of the blob):
//
//	byte 0            : format version (currently keyBlobVersion1) — a
//	                     builder judgment call, not spelled out in the
//	                     docs, added so a future change to the KDF/AEAD
//	                     choice doesn't need a data migration to
//	                     distinguish old rows from new ones.
//	bytes 1..17        : scrypt salt (scryptSaltSize = 16 bytes, random
//	                     per row)
//	bytes 17..29       : AES-GCM nonce (gcmNonceSize = 12 bytes, standard
//	                     size returned by cipher.NewGCM's NonceSize(),
//	                     random per encryption)
//	bytes 29..end      : AES-256-GCM ciphertext of the raw 64-byte Ed25519
//	                     private key, with GCM's 16-byte authentication
//	                     tag appended (as Seal already does) — no separate
//	                     length prefix is needed because every field
//	                     before it has a fixed, known width, so the
//	                     ciphertext is simply "whatever's left."
//
// scrypt parameters (N=32768, r=8, p=1, 32-byte derived key for AES-256)
// are the standard "interactive" cost parameters recommended by the
// scrypt package docs as of this writing — a judgment call since neither
// skill pins exact KDF cost parameters.
const (
	keyBlobVersion1 = 0x01

	scryptSaltSize = 16
	scryptN        = 1 << 15
	scryptR        = 8
	scryptP        = 1
	aesKeySize     = 32 // AES-256

	gcmNonceSize = 12
)

// deriveKey runs scrypt over passphrase+salt to produce an AES-256 key.
func deriveKey(passphrase string, salt []byte) ([]byte, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, aesKeySize)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	return key, nil
}

// encryptPrivateKey packs a random salt+nonce and the AES-256-GCM sealed
// ciphertext of privKey into one blob per this file's documented layout,
// ready to store in signing_keys.private_key_encrypted.
func encryptPrivateKey(privKey ed25519.PrivateKey, passphrase string) ([]byte, error) {
	salt := make([]byte, scryptSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	key, err := deriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("constructing AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("constructing GCM: %w", err)
	}
	if gcm.NonceSize() != gcmNonceSize {
		return nil, fmt.Errorf("unexpected GCM nonce size %d (want %d)", gcm.NonceSize(), gcmNonceSize)
	}

	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, privKey, nil)

	blob := make([]byte, 0, 1+scryptSaltSize+gcmNonceSize+len(ciphertext))
	blob = append(blob, keyBlobVersion1)
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// decryptPrivateKey unpacks a blob produced by encryptPrivateKey and
// returns the original Ed25519 private key. A wrong passphrase derives the
// wrong AES key, which makes GCM's authentication check fail — this
// returns a plain error in that case, never a panic and never silently
// wrong bytes, since AEAD open fails closed.
func decryptPrivateKey(blob []byte, passphrase string) (ed25519.PrivateKey, error) {
	minLen := 1 + scryptSaltSize + gcmNonceSize
	if len(blob) < minLen {
		return nil, fmt.Errorf("decrypting private key: blob too short (%d bytes, want at least %d)", len(blob), minLen)
	}
	if blob[0] != keyBlobVersion1 {
		return nil, fmt.Errorf("decrypting private key: unknown blob format version %d", blob[0])
	}

	offset := 1
	salt := blob[offset : offset+scryptSaltSize]
	offset += scryptSaltSize
	nonce := blob[offset : offset+gcmNonceSize]
	offset += gcmNonceSize
	ciphertext := blob[offset:]

	key, err := deriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("constructing AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("constructing GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting private key: authentication failed (wrong passphrase or corrupt data): %w", err)
	}

	return ed25519.PrivateKey(plaintext), nil
}

// SaveSigningKey encrypts key.PrivateKey under passphrase and inserts a new
// signing_keys row. retire_at is left NULL — it is only ever set later by
// RetireSigningKey (rotation orchestration that decides *when* to retire a
// key is Phase 8, out of scope here).
func (s *Store) SaveSigningKey(ctx context.Context, key *domain.SigningKey, passphrase string) error {
	blob, err := encryptPrivateKey(key.PrivateKey, passphrase)
	if err != nil {
		return fmt.Errorf("saving signing key %s: %w", key.KID, err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO signing_keys (kid, public_key, private_key_encrypted, active, created_at, retire_at)
		VALUES (?, ?, ?, ?, ?, NULL)
	`, key.KID, []byte(key.PublicKey), blob, boolToInt(key.Active), key.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("saving signing key %s: %w", key.KID, err)
	}
	return nil
}

// GetActiveSigningKey returns the current signing key (active = 1),
// decrypting its private key with passphrase. Returns domain.ErrNotFound
// if no active key exists (e.g. before first-boot bootstrap, per
// cdamp-signing's "Key lifecycle" section — deciding what to do about that
// is Phase 4's concern, not this method's; this method only needs to let
// the caller tell "no rows" apart from a real error).
//
// ORDER BY created_at DESC LIMIT 1 is defensive: nothing in the schema
// prevents more than one row from having active = 1 (no unique partial
// index on it), so this picks the most recently created one rather than
// leaving the result under-specified if that ever happens.
func (s *Store) GetActiveSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT kid, public_key, private_key_encrypted, active, created_at
		FROM signing_keys
		WHERE active = 1
		ORDER BY created_at DESC
		LIMIT 1
	`)
	return scanSigningKey(row, passphrase, "getting active signing key")
}

// GetPreviousSigningKey returns the most recent retired-but-still-in-grace
// key (active = 0 AND retire_at > now), decrypting its private key with
// passphrase. Returns domain.ErrNotFound if no such row exists — which is
// the normal, expected state outside a rotation grace period (no rows have
// ever been retired, or every retired row's retire_at has already passed),
// not a distinct error condition from "no rows at all."
//
// Boundary judgment call: retire_at is compared with strict ">" against
// now, matching cdamp-signing's "/.well-known/cdamp/keys response" wording
// that a previous key is "omitted entirely once past retire_at" — the
// docs don't pin down the exact instant retire_at itself falls on, but
// "past retire_at" reads most naturally as excluding the row at the exact
// retire_at second and after, not including it.
func (s *Store) GetPreviousSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx, `
		SELECT kid, public_key, private_key_encrypted, active, created_at
		FROM signing_keys
		WHERE active = 0 AND retire_at > ?
		ORDER BY created_at DESC
		LIMIT 1
	`, now)
	return scanSigningKey(row, passphrase, "getting previous signing key")
}

// scanSigningKey scans one signing_keys row (kid, public_key,
// private_key_encrypted, active, created_at, in that order) and decrypts
// its private key with passphrase. context is used only to prefix error
// messages with which query failed.
func scanSigningKey(row *sql.Row, passphrase string, context string) (*domain.SigningKey, error) {
	var (
		kid                  string
		publicKey, blob      []byte
		activeInt, createdAt int64
	)
	if err := row.Scan(&kid, &publicKey, &blob, &activeInt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("%s: %w", context, err)
	}

	privKey, err := decryptPrivateKey(blob, passphrase)
	if err != nil {
		return nil, fmt.Errorf("%s: decrypting private key for %s: %w", context, kid, err)
	}

	return &domain.SigningKey{
		KID:        kid,
		PublicKey:  ed25519.PublicKey(publicKey),
		PrivateKey: privKey,
		Active:     activeInt != 0,
		CreatedAt:  time.Unix(createdAt, 0).UTC(),
	}, nil
}

// RetireSigningKey marks the signing key identified by kid inactive and
// sets its retire_at timestamp — the plain SQL update used by rotation
// (generating the new active key and flipping which row is active is
// rotation orchestration, Phase 8, not this method's job). Returns
// domain.ErrNotFound if no signing key with the given kid exists.
func (s *Store) RetireSigningKey(ctx context.Context, kid string, retireAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE signing_keys SET active = 0, retire_at = ? WHERE kid = ?
	`, retireAt.Unix(), kid)
	if err != nil {
		return fmt.Errorf("retiring signing key %s: %w", kid, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("retiring signing key %s: %w", kid, err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}
