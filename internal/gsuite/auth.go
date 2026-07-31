package gsuite

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	reports "google.golang.org/api/admin/reports/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
)

// Limits and permissions applied to credential material on disk.
const (
	// maxCredentialFileSize guards against pointing the tool at a huge file by
	// mistake; real key files are a few kilobytes.
	maxCredentialFileSize = 1 << 20 // 1 MiB
	// tokenFileMode keeps the cached OAuth token readable only by its owner. The
	// token grants directory-wide read access, so it is as sensitive as a password.
	tokenFileMode fs.FileMode = 0o600
)

// DefaultScopes returns the least-privilege OAuth scopes AegiSheet needs.
//
// Earlier versions requested admin.AdminDirectoryUserScope, which grants
// *write* access to every user object in the tenant — far beyond what a reporting
// tool requires, and a standing risk if the cached token leaked. Read-only scopes
// are used wherever the API offers them.
//
// AdminDirectoryUserSecurityScope is the exception: listing application specific
// passwords and third-party OAuth tokens requires it and no read-only variant
// exists. Deployments that do not run those two tasks can narrow the set further
// with -scopes.
//
// Changing scopes invalidates a cached token: delete the token file afterwards so
// the next run re-authorises with the new set.
func DefaultScopes() []string {
	return []string{
		admin.AdminDirectoryUserReadonlyScope,
		admin.AdminDirectoryGroupReadonlyScope,
		admin.AdminDirectoryUserSecurityScope,
		reports.AdminReportsAuditReadonlyScope,
	}
}

// NewHTTPClient builds an authenticated HTTP client according to the configured
// credential mode.
//
// The returned client already carries the User-Agent, the per-request timeout and
// debug request logging, and it refreshes (and re-persists) OAuth tokens as
// needed. It is safe for concurrent use.
func NewHTTPClient(
	ctx context.Context,
	auth config.AuthConfig,
	api config.APIConfig,
	log *slog.Logger,
) (*http.Client, error) {
	scopes := auth.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes()
	}

	// The oauth2 library performs its own HTTP calls (token exchange and refresh)
	// using the client stored in the context. Installing ours there means those
	// calls are subject to the same timeout and instrumentation as API calls.
	base := newBaseHTTPClient(api.HTTPTimeout, api.UserAgent, log)
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)

	mode, err := auth.Mode()
	if err != nil {
		return nil, err
	}
	log.Info("authenticating to Google Workspace",
		slog.String("mode", string(mode)),
		slog.Int("scopes", len(scopes)))

	switch mode {
	case config.AuthModeServiceAccount:
		return serviceAccountClient(ctx, auth, scopes, log)
	case config.AuthModeADC:
		return adcClient(ctx, scopes)
	case config.AuthModeInstalledApp:
		return installedAppClient(ctx, auth, scopes, log)
	default:
		return nil, fmt.Errorf("unsupported authentication mode %q", mode)
	}
}

// serviceAccountClient authenticates with a service account key and domain-wide
// delegation. This is the mode intended for scheduled, unattended runs: it needs
// no browser, no cached token and no terminal.
func serviceAccountClient(
	ctx context.Context,
	auth config.AuthConfig,
	scopes []string,
	log *slog.Logger,
) (*http.Client, error) {
	data, err := readCredentialFile(auth.ServiceAccountFile, log)
	if err != nil {
		return nil, err
	}

	jwtConfig, err := google.JWTConfigFromJSON(data, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse service account key %s: %w", auth.ServiceAccountFile, err)
	}

	// Admin SDK calls must be made *as* an administrator. A service account has no
	// Workspace identity of its own, so it impersonates one; without Subject the
	// API answers 401 unauthorized with no useful detail.
	jwtConfig.Subject = auth.ImpersonateSubject

	log.Debug("service account configured",
		slog.String("key_file", auth.ServiceAccountFile),
		slog.String("subject", auth.ImpersonateSubject))

	return jwtConfig.Client(ctx), nil
}

// adcClient authenticates with Application Default Credentials, which is the
// natural choice inside Google Cloud where the runtime supplies credentials.
func adcClient(ctx context.Context, scopes []string) (*http.Client, error) {
	client, err := google.DefaultClient(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf("obtain application default credentials: %w", err)
	}
	return client, nil
}

// installedAppClient authenticates with an OAuth client secret and a cached user
// token, prompting on the terminal when no usable token exists.
func installedAppClient(
	ctx context.Context,
	auth config.AuthConfig,
	scopes []string,
	log *slog.Logger,
) (*http.Client, error) {
	data, err := readCredentialFile(auth.CredentialsFile, log)
	if err != nil {
		return nil, err
	}
	oauthConfig, err := google.ConfigFromJSON(data, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth client secret %s: %w", auth.CredentialsFile, err)
	}

	token, err := loadToken(auth.TokenFile, log)
	switch {
	case err == nil:
		log.Debug("using cached OAuth token", slog.String("token_file", auth.TokenFile))
	case auth.NonInteractive:
		// Failing here is the point of -non-interactive: a scheduled run must not
		// block forever on a terminal prompt that nobody will ever answer.
		return nil, fmt.Errorf("no usable OAuth token in %s and -non-interactive is set "+
			"(authorise once interactively, or switch to -service-account): %w", auth.TokenFile, err)
	default:
		log.Info("no usable OAuth token, starting interactive authorisation",
			slog.String("token_file", auth.TokenFile))
		token, err = authorizeInteractively(ctx, oauthConfig, log)
		if err != nil {
			return nil, err
		}
		if err := saveToken(auth.TokenFile, token); err != nil {
			// The run can continue with an in-memory token; only the next run pays.
			log.Warn("could not cache the OAuth token; the next run will ask again",
				slog.String("token_file", auth.TokenFile), slog.String("error", err.Error()))
		}
	}

	// oauthConfig.TokenSource already refreshes and caches in memory. Wrapping it
	// persists rotated tokens, without which a tenant that rotates refresh tokens
	// breaks every unattended run after the first.
	source := &persistingTokenSource{
		inner:   oauthConfig.TokenSource(ctx, token),
		path:    auth.TokenFile,
		current: token,
		log:     log,
	}
	return oauth2.NewClient(ctx, source), nil
}

// persistingTokenSource writes refreshed tokens back to disk.
//
// It is safe for concurrent use: fan-out report tasks share one HTTP client, and
// several goroutines can therefore request a token at the same moment.
type persistingTokenSource struct {
	inner oauth2.TokenSource
	path  string
	log   *slog.Logger

	mu      sync.Mutex
	current *oauth2.Token
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	token, err := s.inner.Token()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Only touch the disk when the material actually changed.
	if s.current != nil &&
		token.AccessToken == s.current.AccessToken &&
		token.RefreshToken == s.current.RefreshToken {
		return token, nil
	}
	s.current = token

	if err := saveToken(s.path, token); err != nil {
		// Non-fatal: the refreshed token works for this run either way.
		s.log.Warn("could not persist the refreshed OAuth token",
			slog.String("token_file", s.path), slog.String("error", err.Error()))
	} else {
		s.log.Debug("refreshed OAuth token persisted", slog.String("token_file", s.path))
	}
	return token, nil
}

// authorizeInteractively performs the installed-application consent flow on the
// terminal.
//
// The prompt accepts either the bare authorisation code or the entire redirect
// URL. Accepting the URL lets the anti-forgery state parameter be verified, which
// a hand-copied code alone cannot support.
func authorizeInteractively(
	ctx context.Context,
	oauthConfig *oauth2.Config,
	log *slog.Logger,
) (*oauth2.Token, error) {
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	// AccessTypeOffline plus ApprovalForce guarantees a refresh token even when the
	// operator has consented before; without one, every subsequent run would need
	// a human again.
	authURL := oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	// Written to stderr, not through the logger: this is an interactive dialogue
	// with the operator, and it must stay readable regardless of log level or
	// format.
	fmt.Fprintf(os.Stderr, "\nAegiSheet needs to be authorised for this Workspace tenant.\n\n"+
		"  1. Open this URL in a browser signed in as a Workspace administrator:\n\n     %s\n\n"+
		"  2. Complete the consent screen.\n"+
		"  3. Paste the authorisation code, or the full redirect URL, below.\n\n"+
		"authorisation code: ", authURL)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return nil, fmt.Errorf("read authorisation code from standard input: %w", err)
	}

	code, returnedState, err := parseAuthorizationResponse(line)
	if err != nil {
		return nil, err
	}
	// The state can only be checked when the operator pasted the redirect URL.
	if returnedState != "" && returnedState != state {
		return nil, errors.New("authorisation state mismatch: the pasted redirect URL does not belong " +
			"to this authorisation attempt")
	}
	if returnedState == "" {
		log.Debug("authorisation state not verified: only the bare code was supplied")
	}

	token, err := oauthConfig.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange authorisation code for a token: %w", err)
	}
	if token.RefreshToken == "" {
		log.Warn("the token has no refresh component; unattended runs will need authorising again")
	}
	return token, nil
}

// parseAuthorizationResponse extracts the code, and the state when present, from
// whatever the operator pasted.
func parseAuthorizationResponse(input string) (code, state string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", errors.New("no authorisation code supplied")
	}

	// A full redirect URL, or a bare query string.
	if strings.Contains(trimmed, "code=") {
		raw := trimmed
		if idx := strings.Index(raw, "?"); idx >= 0 {
			raw = raw[idx+1:]
		}
		values, parseErr := url.ParseQuery(raw)
		if parseErr == nil && values.Get("code") != "" {
			return values.Get("code"), values.Get("state"), nil
		}
	}

	// Otherwise treat the input as the code itself.
	if strings.ContainsAny(trimmed, " \t") {
		return "", "", errors.New("the authorisation code contains whitespace; paste it without line breaks")
	}
	return trimmed, "", nil
}

// randomState produces the OAuth anti-forgery state parameter.
func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate authorisation state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// loadToken reads a cached OAuth token, rejecting one that cannot be refreshed.
func loadToken(path string, log *slog.Logger) (*oauth2.Token, error) {
	if path == "" {
		return nil, errors.New("no token file configured")
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open token file %s: %w", path, err)
	}
	warnIfPermissive(path, info.Mode(), "OAuth token", log)

	// The path comes from this operator's own flag, environment or configuration
	// file, which are all inputs they already control completely; there is no
	// untrusted source to traverse out of, and constraining a CLI's file arguments
	// to a fixed root would break every legitimate deployment layout.
	// #nosec G304 -- operator-supplied path, by design
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", path, err)
	}

	token := &oauth2.Token{}
	if err := json.Unmarshal(data, token); err != nil {
		return nil, fmt.Errorf("parse token file %s: %w", path, err)
	}

	// A token with neither a live access token nor a refresh token is useless; a
	// clear error beats an opaque 401 from the first API call.
	if token.RefreshToken == "" && !token.Valid() {
		return nil, fmt.Errorf("token in %s is expired and has no refresh component", path)
	}
	return token, nil
}

// saveToken writes the token atomically with owner-only permissions.
func saveToken(path string, token *oauth2.Token) error {
	if path == "" {
		return errors.New("no token file configured")
	}
	// Serialising the secret is the entire purpose of a token cache: without the
	// refresh component on disk, every scheduled run would need a human. The
	// exposure is controlled by where it lands, not by whether it is written —
	// writeFileAtomic creates the file with mode 0600 before any bytes reach it.
	// #nosec G117 -- persisting the token is the intended behaviour; see tokenFileMode
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	return writeFileAtomic(path, data, tokenFileMode)
}

// readCredentialFile loads a credential JSON, with a size bound and a warning
// when the file is more widely readable than it should be.
func readCredentialFile(path string, log *slog.Logger) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no credential file configured")
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open credential file %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("credential file %s is a directory", path)
	}
	if info.Size() > maxCredentialFileSize {
		return nil, fmt.Errorf("credential file %s is %d bytes, which exceeds the %d byte limit",
			path, info.Size(), maxCredentialFileSize)
	}
	warnIfPermissive(path, info.Mode(), "credential", log)

	// Operator-supplied path, as above. The size was bounded before opening, so a
	// mistyped path cannot make the process read something enormous.
	// #nosec G304 -- operator-supplied path, by design
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential file %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("credential file %s is empty", path)
	}
	return data, nil
}

// warnIfPermissive reports credential files that group or others can read.
func warnIfPermissive(path string, mode fs.FileMode, kind string, log *slog.Logger) {
	if mode.Perm()&0o077 != 0 {
		log.Warn("credential file is readable beyond its owner",
			slog.String("path", path),
			slog.String("kind", kind),
			slog.String("mode", fmt.Sprintf("0%o", mode.Perm())),
			slog.String("remedy", "chmod 600 "+path))
	}
}

// writeFileAtomic writes data to path via a temporary file in the same directory,
// so a crash cannot leave a half-written credential behind.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".aegisheet-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	// Tighten permissions before writing: os.CreateTemp already uses 0600, but the
	// requested mode may be stricter still.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("move %s into place at %s: %w", tmpName, path, err)
	}
	tmpName = "" // ownership transferred
	return nil
}
