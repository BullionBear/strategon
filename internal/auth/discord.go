package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	discordAuthorizeURL = "https://discord.com/api/oauth2/authorize"
	discordTokenURL     = "https://discord.com/api/oauth2/token"
	discordMeURL        = "https://discord.com/api/users/@me"
)

type discordTokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Global   string `json:"global_name"`
}

func (s *Service) discordAuthURL(state string) string {
	q := url.Values{}
	q.Set("client_id", s.discordClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", s.discordRedirectURL)
	q.Set("scope", "identify")
	q.Set("state", state)
	q.Set("prompt", "consent")
	return discordAuthorizeURL + "?" + q.Encode()
}

func (s *Service) exchangeDiscordCode(code string) (*User, error) {
	form := url.Values{}
	form.Set("client_id", s.discordClientID)
	form.Set("client_secret", s.discordClientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.discordRedirectURL)

	req, err := http.NewRequest(http.MethodPost, discordTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr discordTokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("discord token decode: %w", err)
	}
	if tr.AccessToken == "" {
		if tr.Error != "" {
			return nil, fmt.Errorf("discord token: %s (%s)", tr.Error, tr.ErrorDesc)
		}
		return nil, fmt.Errorf("discord token: empty access_token (HTTP %d)", resp.StatusCode)
	}

	meReq, err := http.NewRequest(http.MethodGet, discordMeURL, nil)
	if err != nil {
		return nil, err
	}
	meReq.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	meResp, err := client.Do(meReq)
	if err != nil {
		return nil, err
	}
	defer meResp.Body.Close()
	meBody, _ := io.ReadAll(io.LimitReader(meResp.Body, 1<<20))
	var du discordUser
	if err := json.Unmarshal(meBody, &du); err != nil {
		return nil, fmt.Errorf("discord user decode: %w", err)
	}
	if du.ID == "" {
		return nil, fmt.Errorf("discord user: empty id (HTTP %d)", meResp.StatusCode)
	}
	name := du.Username
	if du.Global != "" {
		name = du.Global
	}
	return &User{ID: du.ID, Username: name, Source: SourceDiscord}, nil
}
