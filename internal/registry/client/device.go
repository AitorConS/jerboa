package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
)

// DeviceFlow runs GitHub's OAuth device flow: the user approves a code in a
// browser while the client polls for the token.
type DeviceFlow struct {
	// GitHub is the GitHub web origin; empty means https://github.com.
	GitHub string
	HTTP   *http.Client
	// sleep waits between polls; replaced in tests.
	sleep func(context.Context, time.Duration) error
}

// DeviceCode is what the user enters at VerificationURI.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (f DeviceFlow) origin() string {
	if f.GitHub != "" {
		return strings.TrimRight(f.GitHub, "/")
	}
	return "https://github.com"
}

func (f DeviceFlow) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.origin()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := f.HTTP
	if hc == nil {
		hc = httpclient.Default
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: %s answered HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", path, err)
	}
	return nil
}

// Start asks GitHub for a device code with the read:org scope, which lets the
// registry see the organizations the user may push for.
func (f DeviceFlow) Start(ctx context.Context, clientID string) (DeviceCode, error) {
	var dc DeviceCode
	err := f.post(ctx, "/login/device/code", url.Values{"client_id": {clientID}, "scope": {"read:org"}}, &dc)
	if err == nil && (dc.DeviceCode == "" || dc.UserCode == "") {
		err = fmt.Errorf("github: no device code in the answer")
	}
	return dc, err
}

// Wait polls until the user approves the code, then returns the GitHub token.
func (f DeviceFlow) Wait(ctx context.Context, clientID string, dc DeviceCode) (string, error) {
	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(dc.ExpiresIn, 60)) * time.Second)
	sleep := f.sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	for time.Now().Before(deadline) {
		if err := sleep(ctx, interval); err != nil {
			return "", fmt.Errorf("github: %w", err)
		}
		var out struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			Interval    int    `json:"interval"`
		}
		form := url.Values{
			"client_id":   {clientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		if err := f.post(ctx, "/login/oauth/access_token", form, &out); err != nil {
			return "", err
		}
		switch out.Error {
		case "":
			if out.AccessToken != "" {
				return out.AccessToken, nil
			}
			return "", fmt.Errorf("github: no token in the answer")
		case "authorization_pending":
		case "slow_down":
			interval = time.Duration(max(out.Interval, int(interval/time.Second)+5)) * time.Second
		case "expired_token":
			return "", fmt.Errorf("the code expired before it was approved; run jerboa login again")
		case "access_denied":
			return "", fmt.Errorf("the login was denied in the browser")
		default:
			return "", fmt.Errorf("github: %s", out.Error)
		}
	}
	return "", fmt.Errorf("the code expired before it was approved; run jerboa login again")
}
