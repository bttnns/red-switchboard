package cli

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// teslamate.go is a small helper group for wiring and validating a TeslaMate
// stack that points at redswitchboard, so the integration test (and a human
// poking the stack) needs no browser sign-in and no hand-written SQL:
//
//   teslamate auth  - write the Tesla token row into TeslaMate's Postgres so it
//                     starts polling redswitchboard immediately (no UI sign-in).
//   teslamate check - query positions/drives/charges and assert an expected
//                     vehicle state appeared; exit 0 (pass) or 1 (fail).
//
// Both talk to TeslaMate's Postgres directly. `auth` reproduces TeslaMate's own
// token encryption (Cloak AES-256-GCM keyed by SHA-256 of ENCRYPTION_KEY) so the
// row it writes is one TeslaMate can decrypt.

func newTeslamateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "teslamate",
		Short:   "wire and validate a TeslaMate stack pointed at redswitchboard",
		GroupID: groupInspect,
	}
	cmd.AddCommand(newTeslamateAuthCmd(), newTeslamateCheckCmd())
	return cmd
}

// newTeslamateAuthCmd writes the encrypted Tesla token row so TeslaMate signs in
// against redswitchboard without the one-click UI step.
func newTeslamateAuthCmd() *cobra.Command {
	var dbURL, encKey, token string
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "write TeslaMate's Tesla token row (no browser sign-in)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if encKey == "" {
				encKey = os.Getenv("ENCRYPTION_KEY")
			}
			if encKey == "" {
				return fmt.Errorf("teslamate auth: --encryption-key (or $ENCRYPTION_KEY) is required; it must match TeslaMate's ENCRYPTION_KEY")
			}
			// The qts- prefix makes TeslaMate derive the issuer from TESLA_AUTH_HOST
			// (redswitchboard) instead of decoding the value as a Tesla JWT. The
			// access token is otherwise cosmetic: the sink gates on the ?token= query,
			// not the bearer.
			value := "qts-" + token
			key := sha256.Sum256([]byte(encKey))
			access, err := cloakEncrypt(key[:], []byte(value))
			if err != nil {
				return fmt.Errorf("teslamate auth: encrypt access: %w", err)
			}
			refresh, err := cloakEncrypt(key[:], []byte(value))
			if err != nil {
				return fmt.Errorf("teslamate auth: encrypt refresh: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := pgx.Connect(ctx, dbURL)
			if err != nil {
				return fmt.Errorf("teslamate auth: connect: %w", err)
			}
			defer func() { _ = conn.Close(ctx) }()

			// TeslaMate keeps a single account token row (schema "private"). Replace
			// it so re-running auth is idempotent.
			if _, err := conn.Exec(ctx, "DELETE FROM private.tokens"); err != nil {
				return fmt.Errorf("teslamate auth: clear tokens: %w", err)
			}
			if _, err := conn.Exec(ctx,
				"INSERT INTO private.tokens (access, refresh, inserted_at, updated_at) VALUES ($1, $2, now(), now())",
				access, refresh); err != nil {
				return fmt.Errorf("teslamate auth: insert token: %w", err)
			}
			fmt.Println("teslamate auth: token row written; TeslaMate will sign in against redswitchboard on its next poll")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dbURL, "db", "", "TeslaMate Postgres URL, e.g. postgres://teslamate:pass@localhost:5432/teslamate?sslmode=disable (required)")
	f.StringVar(&encKey, "encryption-key", "", "TeslaMate ENCRYPTION_KEY (default $ENCRYPTION_KEY); must match the TeslaMate container")
	f.StringVar(&token, "token", "local", "provider token suffix; the stored access/refresh value is qts-<token>")
	_ = cmd.MarkFlagRequired("db")
	return cmd
}

// newTeslamateCheckCmd asserts an expected vehicle state landed in TeslaMate's DB.
func newTeslamateCheckCmd() *cobra.Command {
	var dbURL, expect, vin string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "check",
		Short: "assert TeslaMate logged an expected vehicle state (exit 0/1)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			conn, err := pgx.Connect(ctx, dbURL)
			if err != nil {
				return fmt.Errorf("teslamate check: connect: %w", err)
			}
			defer func() { _ = conn.Close(ctx) }()
			return teslamateCheck(ctx, conn, expect, vin)
		},
	}
	f := cmd.Flags()
	f.StringVar(&dbURL, "db", "", "TeslaMate Postgres URL (required)")
	f.StringVar(&expect, "expect", "online", "expected state: online | driving | charging | parked")
	f.StringVar(&vin, "vin", "", "scope the assertion to one VIN (default: the first car TeslaMate knows)")
	f.DurationVar(&timeout, "timeout", 90*time.Second, "how long to wait for the expected state to appear")
	_ = cmd.MarkFlagRequired("db")
	return cmd
}

// pgConn is the slice of pgx used here (so the assert logic is testable).
type pgConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// teslamateCheck polls TeslaMate's DB until the expected state is observed or the
// context deadline passes. It first waits for the car to appear (TeslaMate creates
// it on the first successful poll), then asserts the per-state predicate.
func teslamateCheck(ctx context.Context, conn pgConn, expect, vin string) error {
	predicate, ok := checkPredicates[expect]
	if !ok {
		return fmt.Errorf("teslamate check: unknown --expect %q (want: online | driving | charging | parked)", expect)
	}

	var carID int
	if err := pollUntil(ctx, func() (bool, error) {
		err := conn.QueryRow(ctx,
			"SELECT id FROM cars WHERE ($1 = '' OR vin = $1) ORDER BY id LIMIT 1", vin,
		).Scan(&carID)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		return fmt.Errorf("teslamate check: no car appeared (is auth done and is redswitchboard serving?): %w", err)
	}

	if err := pollUntil(ctx, func() (bool, error) {
		var got bool
		if err := conn.QueryRow(ctx, predicate, carID).Scan(&got); err != nil {
			return false, err
		}
		return got, nil
	}); err != nil {
		return fmt.Errorf("teslamate check: car %d never reached %q within the timeout: %w", carID, expect, err)
	}
	fmt.Printf("teslamate check: OK - car %d is %q\n", carID, expect)
	return nil
}

// checkPredicates maps an expected state to a boolean SQL predicate over $1=car_id.
// "driving" / "charging" key on an open (end_date IS NULL) drive/charge, which is
// the unambiguous in-progress signal; "parked" is known-but-idle.
var checkPredicates = map[string]string{
	"online": `SELECT EXISTS(SELECT 1 FROM positions WHERE car_id = $1)`,
	"driving": `SELECT EXISTS(SELECT 1 FROM drives WHERE car_id = $1 AND end_date IS NULL)
	            OR EXISTS(SELECT 1 FROM positions WHERE car_id = $1 AND speed > 0 AND date > now() - interval '10 minutes')`,
	"charging": `SELECT EXISTS(SELECT 1 FROM charging_processes WHERE car_id = $1 AND end_date IS NULL)
	             OR EXISTS(SELECT 1 FROM charging_processes cp JOIN charges c ON c.charging_process_id = cp.id
	                       WHERE cp.car_id = $1 AND c.date > now() - interval '10 minutes')`,
	"parked": `SELECT EXISTS(SELECT 1 FROM positions WHERE car_id = $1)
	           AND NOT EXISTS(SELECT 1 FROM drives WHERE car_id = $1 AND end_date IS NULL)
	           AND NOT EXISTS(SELECT 1 FROM charging_processes WHERE car_id = $1 AND end_date IS NULL)`,
}

// pollUntil calls fn every 2s until it returns true or ctx is done.
func pollUntil(ctx context.Context, fn func() (bool, error)) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// cloakEncrypt reproduces TeslaMate's Cloak AES-256-GCM "AES.GCM.V1" format so the
// written token decrypts in TeslaMate. The on-wire blob is:
//
//	<<1, len(tag)>> <> tag <> iv(12) <> ciphertag(16) <> ciphertext
//
// with tag = "AES.GCM.V1" used as the GCM additional-authenticated-data, and the
// key = SHA-256(ENCRYPTION_KEY). See TeslaMate's lib/teslamate/vault.ex.
func cloakEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key) // 32-byte key -> AES-256
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	const tag = "AES.GCM.V1"
	sealed := gcm.Seal(nil, iv, plaintext, []byte(tag)) // ciphertext || ciphertag(16)
	ciphertext, ciphertag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]

	out := make([]byte, 0, 2+len(tag)+len(iv)+len(ciphertag)+len(ciphertext))
	out = append(out, 1, byte(len(tag)))
	out = append(out, tag...)
	out = append(out, iv...)
	out = append(out, ciphertag...)
	out = append(out, ciphertext...)
	return out, nil
}
