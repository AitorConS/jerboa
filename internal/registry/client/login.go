package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DeviceLogin is a sign-in the registry runs through GitHub's device flow:
// the user enters UserCode at VerificationURI while the client polls the
// registry with LoginID. The GitHub token stays with the registry.
type DeviceLogin struct {
	LoginID         string `json:"login_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// StartLogin starts a sign-in on a registry whose discovery names LoginDevice.
func (c *Client) StartLogin(ctx context.Context) (DeviceLogin, error) {
	var l DeviceLogin
	err := c.do(ctx, http.MethodPost, c.Base+"/auth/device", "", nil, "", &l)
	if err == nil && (l.LoginID == "" || l.UserCode == "" || l.VerificationURI == "") {
		err = fmt.Errorf("registry: no login code in the answer")
	}
	return l, err
}

// WaitLogin polls until the user answers on GitHub, then returns the
// registry's refresh token and the account's login. It waits the interval
// the registry asks for between polls, and gives up once the code expires.
func (c *Client) WaitLogin(ctx context.Context, l DeviceLogin) (token, login string, err error) {
	interval := time.Duration(max(l.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(l.ExpiresIn, 60)) * time.Second)
	sleep := c.sleep
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
			return "", "", fmt.Errorf("registry: %w", err)
		}
		// 202 answers {"status": "pending", "interval"}; 200 the login.
		var out struct {
			Status   string `json:"status"`
			Interval int    `json:"interval"`
			Token    string `json:"token"`
			Login    string `json:"login"`
		}
		err := c.do(ctx, http.MethodPost, c.Base+"/auth/device/token", "", jsonBody(map[string]string{"login_id": l.LoginID}), "application/json", &out)
		switch {
		case err == nil && out.Token != "":
			return out.Token, out.Login, nil
		case err == nil && out.Status == "pending":
			if out.Interval > 0 {
				interval = time.Duration(out.Interval) * time.Second
			}
		case err == nil:
			return "", "", errors.New("registry: no token in the answer")
		case IsStatus(err, http.StatusForbidden):
			return "", "", fmt.Errorf("the login was denied in the browser: %w", err)
		case IsStatus(err, http.StatusGone):
			return "", "", fmt.Errorf("the code expired before it was approved; run jerboa login again: %w", err)
		case IsStatus(err, http.StatusNotFound):
			return "", "", fmt.Errorf("the registry no longer knows this login; run jerboa login again: %w", err)
		default:
			return "", "", err
		}
	}
	return "", "", errors.New("the code expired before it was approved; run jerboa login again")
}
