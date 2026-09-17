// Package auth implements GitHub's OAuth device flow and local token storage.
//
// The device flow needs no client secret and no callback URL, which is what
// lets herdr-huddle ship a compiled-in client_id and nothing else.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub's device-flow endpoints live on github.com, not api.github.com.
const (
	DefaultDeviceCodeURL = "https://github.com/login/device/code"
	DefaultTokenURL      = "https://github.com/login/oauth/access_token"
)

// defaultInterval is used when GitHub omits interval from the device-code
// response. The spec's recommended minimum is 5s.
const defaultInterval = 5 * time.Second

// defaultExpiresIn is used when GitHub omits expires_in. Without a default the
// deadline would be "now", and Wait would report ErrExpired before polling
// once.
const defaultExpiresIn = 15 * time.Minute

// slowDownStep is how much the interval grows on slow_down. GitHub asks for at
// least 5s.
const slowDownStep = 5 * time.Second

var (
	// ErrAccessDenied means the user declined the request in the browser.
	ErrAccessDenied = errors.New("auth: the authorization request was denied")
	// ErrExpired means the device code timed out before the user approved it.
	ErrExpired = errors.New("auth: the device code expired before it was approved")
	// ErrDeviceFlowFailed covers protocol errors that are neither of the above.
	ErrDeviceFlowFailed = errors.New("auth: device flow failed")
)

// DeviceFlow is one in-flight authorization. The zero value is not usable:
// ClientID is required.
type DeviceFlow struct {
	ClientID string
	Scopes   []string
	HTTP     *http.Client

	DeviceCodeURL string
	TokenURL      string

	// Now and Sleep are injectable so the polling loop's timing is testable
	// without real waits.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// DeviceCode is what the operator has to be shown: the short code to type and
// where to type it.
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
	Interval        time.Duration
}

// Token is the result of a completed device flow.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

func (f *DeviceFlow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *DeviceFlow) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *DeviceFlow) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

func (f *DeviceFlow) deviceCodeURL() string {
	if f.DeviceCodeURL != "" {
		return f.DeviceCodeURL
	}
	return DefaultDeviceCodeURL
}

func (f *DeviceFlow) tokenURL() string {
	if f.TokenURL != "" {
		return f.TokenURL
	}
	return DefaultTokenURL
}

// statusError is a non-2xx response from GitHub's OAuth endpoints. It is
// typed so callers can explain a specific status: a 404 from the device-code
// endpoint means the client id is wrong, which is otherwise reported as a bare
// "Not Found".
type statusError struct {
	Endpoint string
	Status   int
	Body     string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("auth: %s returned %d: %s", e.Endpoint, e.Status, e.Body)
}

// postForm sends a form-encoded POST asking for a JSON response.
func (f *DeviceFlow) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("auth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub answers form-encoded, which is easy to misparse.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "herdr-huddle")

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("auth: %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("auth: read %s: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{Endpoint: endpoint, Status: resp.StatusCode, Body: summarise(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("auth: decode %s: %w", endpoint, err)
	}
	return nil
}

// summarise keeps a non-JSON error body out of logs and terminal output.
func summarise(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "<") {
		return "non-JSON response body"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "empty response body"
	}
	return s
}

// Start asks GitHub for a device code and the URL to show the operator.
func (f *DeviceFlow) Start(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(f.ClientID) == "" {
		return DeviceCode{}, errors.New("auth: no client_id configured; this is a build error, not a user error")
	}

	form := url.Values{"client_id": {f.ClientID}}
	if len(f.Scopes) > 0 {
		form.Set("scope", strings.Join(f.Scopes, " "))
	}

	var resp struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := f.postForm(ctx, f.deviceCodeURL(), form, &resp); err != nil {
		// A valid client id never 404s here, so a 404 means the client id is
		// wrong. Saying only "Not Found" would send the operator looking for a
		// network problem.
		var status *statusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return DeviceCode{}, fmt.Errorf("%w: GitHub does not recognise the client_id %q — check that the OAuth App exists and has Device Flow enabled", ErrDeviceFlowFailed, f.ClientID)
		}
		return DeviceCode{}, err
	}
	if resp.Error != "" {
		return DeviceCode{}, fmt.Errorf("%w: %s", ErrDeviceFlowFailed, resp.Error)
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("%w: device code response was missing device_code or user_code", ErrDeviceFlowFailed)
	}

	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	expiresIn := time.Duration(resp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}
	return DeviceCode{
		DeviceCode:      resp.DeviceCode,
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		ExpiresIn:       expiresIn,
		Interval:        interval,
	}, nil
}

// Wait polls until the operator approves, denies, or the code expires. It
// never returns a token on any other outcome.
func (f *DeviceFlow) Wait(ctx context.Context, code DeviceCode) (Token, error) {
	if strings.TrimSpace(f.ClientID) == "" {
		return Token{}, errors.New("auth: no client_id configured")
	}

	deadline := f.now().Add(code.ExpiresIn)
	interval := code.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	for {
		// Checked before sleeping so the loop cannot outlive the deadline by
		// one interval.
		if !f.now().Before(deadline) {
			return Token{}, ErrExpired
		}
		if err := f.sleep(ctx, interval); err != nil {
			return Token{}, err
		}

		var resp struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Scope       string `json:"scope"`
			Error       string `json:"error"`
		}
		form := url.Values{
			"client_id":   {f.ClientID},
			"device_code": {code.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		if err := f.postForm(ctx, f.tokenURL(), form, &resp); err != nil {
			return Token{}, err
		}

		switch resp.Error {
		case "":
			if resp.AccessToken == "" {
				return Token{}, fmt.Errorf("%w: response had no access_token", ErrDeviceFlowFailed)
			}
			return Token{
				AccessToken: resp.AccessToken,
				TokenType:   resp.TokenType,
				Scope:       resp.Scope,
			}, nil
		case "authorization_pending":
			// The operator has not finished in the browser yet.
			continue
		case "slow_down":
			interval += slowDownStep
			continue
		case "access_denied":
			return Token{}, ErrAccessDenied
		case "expired_token":
			return Token{}, ErrExpired
		default:
			return Token{}, fmt.Errorf("%w: %s", ErrDeviceFlowFailed, resp.Error)
		}
	}
}
