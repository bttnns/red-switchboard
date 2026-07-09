//go:build integration

// Package integration holds the compose-based end-to-end test: it stands up the
// real stack (mock upstream + redswitchboard + stock TeslaMate + Postgres),
// points TeslaMate at redswitchboard with `redswitchboard teslamate auth` (no
// browser sign-in), drives the mock, and asserts the data landed in TeslaMate's
// DB with `redswitchboard teslamate check`. It proves the whole provider contract
// end to end through the shipped binary.
//
// It needs Docker (or a compatible `docker compose`). Run it with:
//
//	make integration            # go test -tags integration ./test/integration/...
//
// It is excluded from the default `go test ./...` by the integration build tag.
package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	project    = "rsb-integration"
	encKey     = "integration-test-encryption-key"
	dbURL      = "postgres://teslamate:teslamate@database:5432/teslamate?sslmode=disable"
	teslamateU = "http://localhost:4000"
	mockURL    = "http://localhost:5050"
)

// composeEnv is the env every compose call interpolates (overrides any .env).
var composeEnv = []string{
	"ENCRYPTION_KEY=" + encKey,
	"TM_DB_USER=teslamate", "TM_DB_PASS=teslamate", "TM_DB_NAME=teslamate", "TM_DB_HOST=database",
	"TESLA_API_HOST=http://redswitchboard:4000", "TESLA_AUTH_HOST=http://redswitchboard:4000",
	"TESLA_AUTH_PATH=/api/oauth2/v3", "TOKEN=?token=local", "REDSWITCHBOARD_TOKEN=local",
	"GRAFANA_USER=admin", "GRAFANA_PASS=admin",
}

func TestTeslaMateEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found; skipping compose integration test")
	}
	root := repoRoot(t)
	files := []string{
		filepath.Join(root, "examples/rivian-to-teslamate/compose.yaml"),
		filepath.Join(root, "examples/rivian-to-teslamate/compose.dev.yaml"),
	}

	// Always tear the stack (and its volumes) down, even on failure.
	t.Cleanup(func() {
		if out, err := compose(root, files, 3*time.Minute, "down", "-v"); err != nil {
			t.Logf("teardown: %v\n%s", err, out)
		}
	})

	// 1. Bring up the whole stack (builds the redswitchboard image).
	if out, err := compose(root, files, 8*time.Minute, "up", "-d", "--build"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}

	// 2. Wait for TeslaMate's web UI to answer (it has run its DB migrations by then,
	//    so the private.tokens table exists for `auth`).
	waitHTTP(t, teslamateU, 4*time.Minute)

	// 3. Point TeslaMate at redswitchboard with no browser sign-in.
	if out, err := execRSB(root, files, 1*time.Minute,
		"teslamate", "auth", "--db", dbURL, "--encryption-key", encKey, "--token", "local",
	); err != nil {
		t.Fatalf("teslamate auth: %v\n%s", err, out)
	}

	// 4. Drive the mock (the dev mock also auto-cycles, but nudge it explicitly).
	postMock(t, "/mock/scenario/driving")

	// 5. Assert TeslaMate is logging us, then that a drive landed. `check` polls the
	//    DB until the predicate holds or the timeout passes, exiting non-zero on miss.
	if out, err := execRSB(root, files, 3*time.Minute,
		"teslamate", "check", "--db", dbURL, "--expect", "online", "--timeout", "150s",
	); err != nil {
		t.Fatalf("teslamate check --expect online: %v\n%s", err, out)
	}
	if out, err := execRSB(root, files, 3*time.Minute,
		"teslamate", "check", "--db", dbURL, "--expect", "driving", "--timeout", "150s",
	); err != nil {
		t.Fatalf("teslamate check --expect driving: %v\n%s", err, out)
	}
}

// compose runs `docker compose -p <project> -f ... <args>` rooted at the repo (so
// .env and relative volume mounts resolve), with composeEnv applied, and returns
// the combined output.
func compose(root string, files []string, timeout time.Duration, args ...string) ([]byte, error) {
	full := []string{"compose", "-p", project, "--project-directory", root}
	for _, f := range files {
		full = append(full, "-f", f)
	}
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), composeEnv...)
	return cmd.CombinedOutput()
}

// execRSB runs `redswitchboard <args>` inside the running redswitchboard container.
func execRSB(root string, files []string, timeout time.Duration, args ...string) ([]byte, error) {
	return compose(root, files, timeout, append([]string{"exec", "-T", "redswitchboard", "redswitchboard"}, args...)...)
}

// waitHTTP polls url until it answers any HTTP status, or fails the test.
func waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // simple liveness poll
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s did not respond within %s", url, timeout)
}

// postMock POSTs to the mock control surface (best-effort; the auto-cycle drives
// the car regardless).
func postMock(t *testing.T, path string) {
	t.Helper()
	resp, err := http.Post(mockURL+path, "application/json", strings.NewReader("")) //nolint:noctx
	if err != nil {
		t.Logf("post mock %s: %v (continuing; auto-cycle still drives)", path, err)
		return
	}
	_ = resp.Body.Close()
}

// repoRoot returns the module root (two levels up from this test file).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve repo root")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
