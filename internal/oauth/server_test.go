package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
	"github.com/goodtune/whatsapp-mcp/internal/db"
)

const clientRedirect = "https://client.example/callback"

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	oauth  *Server
	auth   *auth.Service
	http   *http.Client
	ghUser struct {
		id    int64
		login string
	}
	clientID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	sqlDB, dialect, err := db.Open("sqlite:" + filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(ctx, sqlDB, dialect); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, auth: auth.NewService(sqlDB, "test-secret")}
	h.ghUser.id, h.ghUser.login = 42, "Octocat"

	// A stand-in for GitHub's token and user endpoints.
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.PostForm.Get("code") != "gh-code" || r.PostForm.Get("code_verifier") == "" {
				http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"gh-token","token_type":"bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer gh-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": h.ghUser.id, "login": h.ghUser.login})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	mux := http.NewServeMux()
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	h.oauth = New(Config{
		BaseURL:            h.srv.URL,
		ResourceURL:        h.srv.URL + "/mcp",
		GitHubClientID:     "gh-client",
		GitHubClientSecret: "gh-secret",
		AllowedUsers:       []string{"octocat", " someone-else "},
	}, sqlDB, h.auth)
	h.oauth.github.Endpoint = oauth2.Endpoint{AuthURL: github.URL + "/authorize", TokenURL: github.URL + "/token"}
	h.oauth.githubUserURL = github.URL + "/user"
	h.oauth.Register(mux)

	jar, _ := cookiejar.New(nil)
	h.http = &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var reg struct {
		ClientID string `json:"client_id"`
		Method   string `json:"token_endpoint_auth_method"`
	}
	resp := h.postJSON("/oauth/register", `{"client_name":"Test Client","redirect_uris":["`+clientRedirect+`"]}`, http.StatusCreated)
	json.NewDecoder(resp.Body).Decode(&reg)
	if reg.ClientID == "" || reg.Method != "none" {
		t.Fatalf("registration = %+v", reg)
	}
	h.clientID = reg.ClientID
	return h
}

func (h *harness) postJSON(path, body string, want int) *http.Response {
	h.t.Helper()
	resp, err := h.http.Post(h.srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != want {
		h.t.Fatalf("POST %s: HTTP %d, want %d", path, resp.StatusCode, want)
	}
	return resp
}

func (h *harness) do(method, path string, form url.Values) *http.Response {
	h.t.Helper()
	var resp *http.Response
	var err error
	if method == http.MethodGet {
		resp, err = h.http.Get(h.srv.URL + path)
	} else {
		resp, err = h.http.PostForm(h.srv.URL+path, form)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize runs the browser side of the flow up to the consent page and
// returns the consent id.
func (h *harness) authorize(verifier string) string {
	h.t.Helper()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.clientID},
		"redirect_uri":          {clientRedirect},
		"state":                 {"client-state"},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {h.srv.URL + "/mcp"},
	}
	resp := h.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("authorize: HTTP %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasSuffix(loc.Path, "/authorize") || loc.Query().Get("client_id") != "gh-client" ||
		loc.Query().Get("code_challenge") == "" {
		h.t.Fatalf("authorize redirected to %s", loc)
	}

	resp = h.do(http.MethodGet, "/oauth/github/callback?code=gh-code&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	page := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("callback: HTTP %d: %s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "Test Client") || !strings.Contains(page, "client.example") {
		h.t.Fatalf("consent page doesn't name the client and redirect host:\n%s", page)
	}
	m := regexp.MustCompile(`name="consent_id" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		h.t.Fatalf("no consent_id in page:\n%s", page)
	}
	return m[1]
}

func (h *harness) approve(consentID string) string {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/oauth/consent", url.Values{"consent_id": {consentID}, "action": {"approve"}})
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("consent: HTTP %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), clientRedirect+"?") || loc.Query().Get("state") != "client-state" ||
		loc.Query().Get("iss") != h.srv.URL {
		h.t.Fatalf("consent redirected to %s", loc)
	}
	return loc.Query().Get("code")
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func (h *harness) token(form url.Values) (int, tokenResponse) {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/oauth/token", form)
	defer resp.Body.Close()
	var tr tokenResponse
	json.NewDecoder(resp.Body).Decode(&tr)
	return resp.StatusCode, tr
}

func (h *harness) exchange(code, verifier string) (int, tokenResponse) {
	return h.token(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {clientRedirect},
		"client_id":     {h.clientID},
		"code_verifier": {verifier},
	})
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestFullFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// An account from before OAuth is claimed by the same GitHub login.
	if _, err := h.oauth.db.ExecContext(ctx, `INSERT INTO users (username) VALUES ('Octocat')`); err != nil {
		t.Fatal(err)
	}
	var existingID int64
	h.oauth.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'Octocat'`).Scan(&existingID)

	verifier := "a-sufficiently-long-pkce-code-verifier-0123456789"
	code := h.approve(h.authorize(verifier))

	status, tr := h.exchange(code, verifier)
	if status != http.StatusOK || tr.AccessToken == "" || tr.RefreshToken == "" || tr.ExpiresIn <= 0 {
		t.Fatalf("token exchange: HTTP %d %+v", status, tr)
	}
	userID, _, err := h.auth.VerifyToken(tr.AccessToken, h.srv.URL+"/mcp")
	if err != nil || userID != existingID {
		t.Fatalf("VerifyToken = %d, %v; want user %d", userID, err, existingID)
	}
	if _, _, err := h.auth.VerifyToken(tr.AccessToken, "https://other.example/mcp"); err == nil {
		t.Fatal("token accepted for a different audience")
	}

	// Codes are single-use.
	if status, tr := h.exchange(code, verifier); status != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("code reuse: HTTP %d %+v", status, tr)
	}

	// Refresh tokens rotate and are single-use.
	refresh := func(rt string) (int, tokenResponse) {
		return h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {h.clientID}})
	}
	status, tr2 := refresh(tr.RefreshToken)
	if status != http.StatusOK || tr2.AccessToken == "" || tr2.RefreshToken == tr.RefreshToken {
		t.Fatalf("refresh: HTTP %d %+v", status, tr2)
	}
	if status, tr3 := refresh(tr.RefreshToken); status != http.StatusBadRequest || tr3.Error != "invalid_grant" {
		t.Fatalf("refresh reuse: HTTP %d %+v", status, tr3)
	}
	// A refresh token only works for the client it was issued to.
	if status, _ := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr2.RefreshToken}, "client_id": {"other"}}); status != http.StatusBadRequest {
		t.Fatalf("refresh by other client: HTTP %d", status)
	}
}

func TestWrongPKCEVerifier(t *testing.T) {
	h := newHarness(t)
	code := h.approve(h.authorize("the-real-verifier-0123456789-0123456789-0123456789"))
	if status, tr := h.exchange(code, "a-different-verifier-0123456789-0123456789-01234567"); status != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("wrong verifier: HTTP %d %+v", status, tr)
	}
}

func TestDeny(t *testing.T) {
	h := newHarness(t)
	consentID := h.authorize("verifier-0123456789-0123456789-0123456789-0123456789")
	resp := h.do(http.MethodPost, "/oauth/consent", url.Values{"consent_id": {consentID}, "action": {"deny"}})
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny: HTTP %d to %s", resp.StatusCode, loc)
	}
	// The consent can't be replayed to approve after all.
	resp = h.do(http.MethodPost, "/oauth/consent", url.Values{"consent_id": {consentID}, "action": {"approve"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed consent: HTTP %d", resp.StatusCode)
	}
}

func TestUserNotAllowed(t *testing.T) {
	h := newHarness(t)
	h.ghUser.id, h.ghUser.login = 7, "mallory"
	q := url.Values{
		"response_type": {"code"}, "client_id": {h.clientID}, "redirect_uri": {clientRedirect},
		"code_challenge": {challenge("v")}, "code_challenge_method": {"S256"},
	}
	resp := h.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp = h.do(http.MethodGet, "/oauth/github/callback?code=gh-code&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed user: HTTP %d", resp.StatusCode)
	}
	var n int
	h.oauth.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 0 {
		t.Fatalf("disallowed user got an account")
	}
}

func TestCallbackFromAnotherBrowser(t *testing.T) {
	h := newHarness(t)
	q := url.Values{
		"response_type": {"code"}, "client_id": {h.clientID}, "redirect_uri": {clientRedirect},
		"code_challenge": {challenge("v")}, "code_challenge_method": {"S256"},
	}
	resp := h.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))

	// No cookie: e.g. a victim sent a GitHub link started by someone else.
	other := &http.Client{CheckRedirect: h.http.CheckRedirect}
	resp, err := other.Get(h.srv.URL + "/oauth/github/callback?code=gh-code&state=" + url.QueryEscape(loc.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without cookie: HTTP %d", resp.StatusCode)
	}
}

func TestAuthorizeRejectsBadRequests(t *testing.T) {
	h := newHarness(t)
	base := url.Values{
		"response_type": {"code"}, "client_id": {h.clientID}, "redirect_uri": {clientRedirect},
		"code_challenge": {challenge("v")}, "code_challenge_method": {"S256"},
	}
	with := func(k, v string) string {
		q := url.Values{}
		for kk, vv := range base {
			q[kk] = vv
		}
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
		return "/oauth/authorize?" + q.Encode()
	}

	// Bad client or redirect URI: shown here, never redirected.
	for _, path := range []string{with("client_id", "nope"), with("redirect_uri", "https://evil.example/cb")} {
		if resp := h.do(http.MethodGet, path, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", path, resp.StatusCode)
		}
	}
	// Other errors go back to the client.
	for path, want := range map[string]string{
		with("code_challenge", ""):                "invalid_request",
		with("code_challenge_method", "plain"):    "invalid_request",
		with("response_type", "token"):            "unsupported_response_type",
		with("resource", "https://other.example"): "invalid_target",
	} {
		resp := h.do(http.MethodGet, path, nil)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc.Query().Get("error") != want {
			t.Errorf("%s: HTTP %d to %s, want error %s", path, resp.StatusCode, loc, want)
		}
	}
}

func TestRedirectURIRules(t *testing.T) {
	for uri, ok := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://127.0.0.1:33418/callback":         true,
		"http://localhost/cb":                     true,
		"cursor://anysphere.cursor-mcp/oauth":     true,
		"http://example.com/cb":                   false,
		"javascript:alert(1)":                     false,
		"https://example.com/cb#frag":             false,
		"/relative":                               false,
	} {
		if err := validateRedirectURI(uri); (err == nil) != ok {
			t.Errorf("validateRedirectURI(%q) = %v, want ok=%v", uri, err, ok)
		}
	}

	registered := []string{"http://127.0.0.1:1234/callback", "https://app.example/cb"}
	for uri, ok := range map[string]bool{
		"http://127.0.0.1:5555/callback": true, // loopback: any port
		"http://127.0.0.1:5555/other":    false,
		"https://app.example/cb":         true,
		"https://app.example/cb2":        false,
		"https://app.example:8443/cb":    false,
	} {
		if got := redirectURIAllowed(registered, uri); got != ok {
			t.Errorf("redirectURIAllowed(%q) = %v, want %v", uri, got, ok)
		}
	}
}

func TestMetadata(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		var prm struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		resp := h.do(http.MethodGet, path, nil)
		json.NewDecoder(resp.Body).Decode(&prm)
		if prm.Resource != h.srv.URL+"/mcp" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != h.srv.URL {
			t.Fatalf("%s = %+v", path, prm)
		}
	}
	if got := h.oauth.ResourceMetadataURL(); got != h.srv.URL+"/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("ResourceMetadataURL = %s", got)
	}

	var asm map[string]any
	resp := h.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	json.NewDecoder(resp.Body).Decode(&asm)
	if asm["issuer"] != h.srv.URL || asm["token_endpoint"] != h.srv.URL+"/oauth/token" {
		t.Fatalf("authorization server metadata = %v", asm)
	}
}
