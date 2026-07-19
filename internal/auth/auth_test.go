package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestParseMode(t *testing.T) {
	m, err := ParseMode("DISCORD")
	if err != nil || m != ModeDiscord {
		t.Fatalf("got %q %v", m, err)
	}
	if _, err := ParseMode("ldap"); err == nil {
		t.Fatal("expected error")
	}
}

func TestActorFormats(t *testing.T) {
	cases := []struct {
		u    *User
		want string
	}{
		{nil, "anonymous"},
		{&User{Username: "local", Source: SourceNone}, "local"},
		{&User{ID: "1", Username: "alice", Source: SourceDiscord}, "alice (discord:1)"},
		{&User{ID: "1", Username: "alice", Source: SourceAPIToken, TokenID: "abcd1234ffff"}, "alice (discord:1 api-token:abcd1234)"},
		{&User{ID: "m", Username: "dev", Source: SourceMock}, "dev (mock)"},
	}
	for _, tc := range cases {
		if got := tc.u.Actor(); got != tc.want {
			t.Fatalf("Actor()=%q want %q", got, tc.want)
		}
	}
}

func TestModeNoneInterceptorInjectsMockUser(t *testing.T) {
	svc, err := New(Config{Mode: ModeNone, MockUser: "local", SessionSecret: "test-secret-at-least-32-bytes-long!!"})
	if err != nil {
		t.Fatal(err)
	}
	var got *User
	interceptor := &streamInterceptor{s: svc}
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		got, _ = UserFromContext(ctx)
		return nil, nil
	}
	req := connect.NewRequest(&struct{}{})
	if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Username != "local" || got.Actor() != "local" {
		t.Fatalf("unexpected user %#v", got)
	}
}

func TestMockLoginExchangeAndAPIToken(t *testing.T) {
	svc, err := New(Config{
		Mode:          ModeMock,
		MockUser:      "dev",
		MockID:        "dev-1",
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		FrontendURL:   "http://127.0.0.1:5173",
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Mock login (JSON) returns access_token.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/mock-login", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
		User        struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	if login.AccessToken == "" || login.User.Username != "dev" {
		t.Fatalf("bad login %#v", login)
	}

	// Create API token with session bearer.
	body := strings.NewReader(`{"name":"ci"}`)
	tokReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/tokens", body)
	tokReq.Header.Set("Authorization", "Bearer "+login.AccessToken)
	tokReq.Header.Set("Content-Type", "application/json")
	tokResp, err := http.DefaultClient.Do(tokReq)
	if err != nil {
		t.Fatal(err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != 201 {
		t.Fatalf("create token status %d", tokResp.StatusCode)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "str_live_") {
		t.Fatalf("token %q", created.Token)
	}

	u, err := svc.userFromBearer("Bearer " + created.Token)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "dev-1" || u.Source != SourceAPIToken {
		t.Fatalf("unexpected token user %#v", u)
	}
	if !strings.Contains(u.Actor(), "api-token:") {
		t.Fatalf("actor %q", u.Actor())
	}
}

func TestMockModeRequiresAuth(t *testing.T) {
	svc, err := New(Config{Mode: ModeMock, SessionSecret: "test-secret-at-least-32-bytes-long!!"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.authenticate("", "")
	if err == nil {
		t.Fatal("expected unauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code %v", connect.CodeOf(err))
	}
}

func TestDiscordModeRequiresConfig(t *testing.T) {
	if _, err := New(Config{Mode: ModeDiscord}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBrowserExchange(t *testing.T) {
	svc, err := New(Config{
		Mode:          ModeMock,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		FrontendURL:   "http://127.0.0.1:5173",
		MockUser:      "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/auth/mock-login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	const marker = "#auth_exchange="
	idx := strings.Index(loc, marker)
	if idx < 0 {
		t.Fatalf("location %q", loc)
	}
	code := loc[idx+len(marker):]

	exReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/exchange", strings.NewReader(`{"code":"`+code+`"}`))
	exReq.Header.Set("Content-Type", "application/json")
	exResp, err := http.DefaultClient.Do(exReq)
	if err != nil {
		t.Fatal(err)
	}
	defer exResp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(exResp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	u, err := svc.userFromBearer("Bearer " + out.AccessToken)
	if err != nil || u.Username != "dev" {
		t.Fatalf("user %#v err %v", u, err)
	}
}
