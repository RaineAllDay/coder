package dbcrypt_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/coderd/database/dbcrypt"
	"github.com/coder/coder/coderd/database/dbfake"
	"github.com/coder/coder/coderd/database/dbgen"
	"github.com/coder/coder/cryptorand"
)

func TestUserLinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("InsertUserLink", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.UserLink(t, crypt, database.UserLink{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		link, err := db.GetUserLinkByLinkedID(ctx, link.LinkedID)
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")
	})

	t.Run("UpdateUserLink", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.UserLink(t, crypt, database.UserLink{})
		_, err := crypt.UpdateUserLink(ctx, database.UpdateUserLinkParams{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
			UserID:            link.UserID,
			LoginType:         link.LoginType,
		})
		require.NoError(t, err)
		link, err = db.GetUserLinkByLinkedID(ctx, link.LinkedID)
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")
	})

	t.Run("GetUserLinkByLinkedID", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.UserLink(t, crypt, database.UserLink{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		link, err := db.GetUserLinkByLinkedID(ctx, link.LinkedID)
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")

		// Reset the key and empty values should be returned!
		initCipher(t, cipher)

		link, err = crypt.GetUserLinkByLinkedID(ctx, link.LinkedID)
		require.ErrorIs(t, err, sql.ErrNoRows)
	})

	t.Run("GetUserLinkByUserIDLoginType", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.UserLink(t, crypt, database.UserLink{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		link, err := db.GetUserLinkByUserIDLoginType(ctx, database.GetUserLinkByUserIDLoginTypeParams{
			UserID:    link.UserID,
			LoginType: link.LoginType,
		})
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")

		// Reset the key and empty values should be returned!
		initCipher(t, cipher)

		link, err = crypt.GetUserLinkByUserIDLoginType(ctx, database.GetUserLinkByUserIDLoginTypeParams{
			UserID:    link.UserID,
			LoginType: link.LoginType,
		})
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}

func TestGitAuthLinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("InsertGitAuthLink", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.GitAuthLink(t, crypt, database.GitAuthLink{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		link, err := db.GetGitAuthLink(ctx, database.GetGitAuthLinkParams{
			ProviderID: link.ProviderID,
			UserID:     link.UserID,
		})
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")
	})

	t.Run("UpdateGitAuthLink", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.GitAuthLink(t, crypt, database.GitAuthLink{})
		_, err := crypt.UpdateGitAuthLink(ctx, database.UpdateGitAuthLinkParams{
			ProviderID:        link.ProviderID,
			UserID:            link.UserID,
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		require.NoError(t, err)
		link, err = db.GetGitAuthLink(ctx, database.GetGitAuthLinkParams{
			ProviderID: link.ProviderID,
			UserID:     link.UserID,
		})
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")
	})

	t.Run("GetGitAuthLink", func(t *testing.T) {
		t.Parallel()
		db, crypt, cipher := setup(t)
		initCipher(t, cipher)
		link := dbgen.GitAuthLink(t, crypt, database.GitAuthLink{
			OAuthAccessToken:  "access",
			OAuthRefreshToken: "refresh",
		})
		link, err := db.GetGitAuthLink(ctx, database.GetGitAuthLinkParams{
			UserID:     link.UserID,
			ProviderID: link.ProviderID,
		})
		require.NoError(t, err)
		requireEncryptedEquals(t, cipher, link.OAuthAccessToken, "access")
		requireEncryptedEquals(t, cipher, link.OAuthRefreshToken, "refresh")

		// Reset the key and empty values should be returned!
		initCipher(t, cipher)

		link, err = crypt.GetGitAuthLink(ctx, database.GetGitAuthLinkParams{
			UserID:     link.UserID,
			ProviderID: link.ProviderID,
		})
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}

func requireEncryptedEquals(t *testing.T, cipher *atomic.Pointer[cryptorand.Cipher], value, expected string) {
	t.Helper()
	c := (*cipher.Load())
	got, err := c.Decrypt([]byte(value[len(dbcrypt.MagicPrefix):]))
	require.NoError(t, err)
	require.Equal(t, expected, string(got))
}

func initCipher(t *testing.T, cipher *atomic.Pointer[cryptorand.Cipher]) {
	t.Helper()
	key := make([]byte, 32) // AES-256 key size is 32 bytes
	_, err := io.ReadFull(rand.Reader, key)
	require.NoError(t, err)
	c, err := cryptorand.CipherAES256(key)
	require.NoError(t, err)
	cipher.Store(&c)
}

func setup(t *testing.T) (db, cryptodb database.Store, cipher *atomic.Pointer[cryptorand.Cipher]) {
	t.Helper()
	rawDB := dbfake.New()
	cipher = &atomic.Pointer[cryptorand.Cipher]{}
	return rawDB, dbcrypt.New(rawDB, &dbcrypt.Options{
		ExternalTokenCipher: cipher,
	}), cipher
}
