package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNoToken means no credential is available from any source.
var ErrNoToken = errors.New("auth: no GitHub token stored; run `herdr-huddle auth login`")

// Environment variables honoured before any stored credential, matching the
// convention every GitHub tool follows.
var tokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// TokenStore persists one GitHub token.
//
// It prefers the OS keychain, addressed through the platform's own command —
// `security` on macOS, `secret-tool` (Secret Service) on Linux — and falls back
// to a 0600 file when neither is present. The fallback is not theoretical: a
// headless Linux box has no Secret Service, so the file is the path that
// actually runs there. Load reports which source answered so callers can warn.
type TokenStore struct {
	Service     string
	Account     string
	FallbackDir string
	GOOS        string

	// Run and Getenv are injectable so tests need no keychain and no
	// environment manipulation.
	Run    func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
	Getenv func(string) string
}

const tokenFileName = "token"

func (s *TokenStore) goos() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtime.GOOS
}

func (s *TokenStore) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if s.Run != nil {
		return s.Run(ctx, stdin, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	// Some keychain tools write diagnostics to stderr; only stdout is the
	// credential.
	cmd.Stderr = nil
	return cmd.Output()
}

func (s *TokenStore) getenv(key string) string {
	if s.Getenv != nil {
		return s.Getenv(key)
	}
	return os.Getenv(key)
}

func (s *TokenStore) filePath() string {
	return filepath.Join(s.FallbackDir, tokenFileName)
}

// Load returns the token and the name of the source that provided it: "env",
// "keychain" or "file".
func (s *TokenStore) Load(ctx context.Context) (string, string, error) {
	for _, key := range tokenEnvVars {
		if v := strings.TrimSpace(s.getenv(key)); v != "" {
			return v, "env", nil
		}
	}
	return s.loadStored(ctx)
}

// loadStored reads a persisted token, ignoring the environment. Delete uses it
// to confirm the token is actually gone.
func (s *TokenStore) loadStored(ctx context.Context) (string, string, error) {
	if v, err := s.loadKeychain(ctx); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), "keychain", nil
	}
	if raw, err := os.ReadFile(s.filePath()); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, "file", nil
		}
	}
	return "", "", ErrNoToken
}

// Save writes the token to the keychain, falling back to a 0600 file. It
// returns the source that actually stored it.
func (s *TokenStore) Save(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("auth: refusing to store an empty token")
	}
	if err := s.saveKeychain(ctx, token); err == nil {
		// The keychain now holds this token, so drop any fallback copy. Leaving
		// it would keep a superseded credential readable on disk after the
		// operator believed they had rotated it.
		_ = os.Remove(s.filePath())
		return "keychain", nil
	}

	if err := os.MkdirAll(s.FallbackDir, 0o700); err != nil {
		return "", fmt.Errorf("auth: create config dir: %w", err)
	}
	// Create with 0600 rather than write-then-chmod: the token must never
	// exist on disk with wider permissions, even briefly.
	f, err := os.OpenFile(s.filePath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("auth: write token file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(token + "\n"); err != nil {
		return "", fmt.Errorf("auth: write token file: %w", err)
	}
	return "file", nil
}

// Delete removes the token from every source it could have come from. It
// reports no error when nothing was stored.
func (s *TokenStore) Delete(ctx context.Context) error {
	keychainErr := s.deleteKeychain(ctx)
	if err := os.Remove(s.filePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth: remove token file: %w", err)
	}

	// Verify rather than trust the exit codes. `security` and `secret-tool`
	// disagree on how to report "not found", so a failed delete is not
	// necessarily a failure — but the operator must never be told the token was
	// removed while it is still retrievable.
	if _, _, err := s.loadStored(ctx); err == nil {
		if keychainErr != nil {
			return fmt.Errorf("auth: a stored token is still present after logout: %w", keychainErr)
		}
		return errors.New("auth: a stored token is still present after logout")
	}
	return nil
}

func (s *TokenStore) loadKeychain(ctx context.Context) (string, error) {
	switch s.goos() {
	case "darwin":
		out, err := s.run(ctx, nil, "security",
			"find-generic-password", "-s", s.Service, "-a", s.Account, "-w")
		return string(out), err
	case "linux":
		out, err := s.run(ctx, nil, "secret-tool",
			"lookup", "service", s.Service, "account", s.Account)
		return string(out), err
	default:
		return "", errors.New("auth: no keychain backend for " + s.goos())
	}
}

func (s *TokenStore) saveKeychain(ctx context.Context, token string) error {
	switch s.goos() {
	case "darwin":
		_, err := s.run(ctx, nil, "security",
			"add-generic-password", "-s", s.Service, "-a", s.Account, "-w", token, "-U")
		return err
	case "linux":
		// secret-tool reads the secret from stdin, keeping it out of the
		// process table.
		_, err := s.run(ctx, []byte(token), "secret-tool",
			"store", "--label", "herdr-huddle "+s.Account,
			"service", s.Service, "account", s.Account)
		return err
	default:
		return errors.New("auth: no keychain backend for " + s.goos())
	}
}

func (s *TokenStore) deleteKeychain(ctx context.Context) error {
	switch s.goos() {
	case "darwin":
		_, err := s.run(ctx, nil, "security",
			"delete-generic-password", "-s", s.Service, "-a", s.Account)
		return err
	case "linux":
		_, err := s.run(ctx, nil, "secret-tool",
			"clear", "service", s.Service, "account", s.Account)
		return err
	default:
		return errors.New("auth: no keychain backend for " + s.goos())
	}
}

// ScopesFor picks the narrowest scope that can open a draft PR in the repo.
// A private repo needs `repo`; that scope is broad by nature, which is why it
// is only requested when there is no narrower option.
func ScopesFor(private bool) []string {
	if private {
		return []string{"repo"}
	}
	return []string{"public_repo"}
}
