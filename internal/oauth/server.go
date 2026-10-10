// Package oauth is a small OAuth 2.1 authorization server for the MCP
// endpoint, following the MCP authorization spec: MCP clients discover it
// via protected resource metadata (RFC 9728) and authorization server
// metadata (RFC 8414), register themselves dynamically (RFC 7591), and use
// the authorization code flow with PKCE.
//
// It doesn't hold any passwords: signing in is delegated to GitHub, and
// only GitHub users on the configured allowlist are let through. After
// GitHub, the user is shown a consent page naming the MCP client and where
// it will be sent the authorization code, so that a client registered by
// someone else can't silently obtain a token for their account (GitHub skips
// its own prompt once a user has authorized the GitHub app).
package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
)

const (
	pendingTTL    = 10 * time.Minute
	requestCookie = "oauth_request"
)

type Config struct {
	// BaseURL is the server's public URL, e.g. https://example.ngrok.app. The
	// issuer, endpoints and GitHub callback URL are all derived from it.
	BaseURL string
	// ResourceURL is the MCP endpoint's URL; access tokens are only valid
	// for it.
	ResourceURL string

	GitHubClientID     string
	GitHubClientSecret string
	// AllowedUsers is the set of GitHub logins allowed to sign in.
	AllowedUsers []string
}

type Server struct {
	cfg     Config
	db      *sql.DB
	auth    *auth.Service
	allowed map[string]bool

	github *oauth2.Config
	// githubUserURL is the GitHub API endpoint for the signed-in user;
	// overridable in tests.
	githubUserURL string

	requests *pendingStore[authRequest]
	consents *pendingStore[consent]
	codes    *pendingStore[authCode]
}

// authRequest is an authorization request waiting on the GitHub sign-in.
type authRequest struct {
	ClientID      string
	ClientName    string
	RedirectURI   string
	State         string
	CodeChallenge string
	// GitHubVerifier is the PKCE verifier for our own request to GitHub.
	GitHubVerifier string
}

// consent is an authorization request waiting for the user to approve it.
type consent struct {
	Request authRequest
	User    auth.User
}

type authCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	UserID        int64
}

func New(cfg Config, db *sql.DB, authService *auth.Service) *Server {
	allowed := make(map[string]bool)
	for _, u := range cfg.AllowedUsers {
		if u = normalizeLogin(u); u != "" {
			allowed[u] = true
		}
	}
	return &Server{
		cfg:     cfg,
		db:      db,
		auth:    authService,
		allowed: allowed,
		github: &oauth2.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			Endpoint:     github.Endpoint,
			RedirectURL:  cfg.BaseURL + "/oauth/github/callback",
		},
		githubUserURL: "https://api.github.com/user",
		requests:      newPendingStore[authRequest](pendingTTL),
		consents:      newPendingStore[consent](pendingTTL),
		codes:         newPendingStore[authCode](pendingTTL),
	}
}

// GitHubCallbackURL is the callback URL to configure on the GitHub OAuth app.
func (s *Server) GitHubCallbackURL() string {
	return s.github.RedirectURL
}

// ResourceMetadataURL is the protected resource metadata URL to advertise in
// the MCP endpoint's WWW-Authenticate challenges.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.BaseURL + "/.well-known/oauth-protected-resource" + resourcePath(s.cfg.ResourceURL)
}

func (s *Server) Register(mux *http.ServeMux) {
	prm := mcpauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.cfg.ResourceURL,
		AuthorizationServers:   []string{s.cfg.BaseURL},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "WhatsApp MCP",
	})
	// RFC 9728 puts the metadata at the well-known path with the resource's
	// path appended; some clients only try the bare well-known path.
	mux.Handle("/.well-known/oauth-protected-resource", prm)
	mux.Handle("/.well-known/oauth-protected-resource"+resourcePath(s.cfg.ResourceURL), prm)

	mux.HandleFunc("/.well-known/oauth-authorization-server", cors(s.handleMetadata))
	mux.HandleFunc("/oauth/register", cors(s.handleRegister))
	mux.HandleFunc("/oauth/token", cors(s.handleToken))
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("GET /oauth/github/callback", s.handleGitHubCallback)
	mux.HandleFunc("POST /oauth/consent", s.handleConsent)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.cfg.BaseURL,
		"authorization_endpoint":                s.cfg.BaseURL + "/oauth/authorize",
		"token_endpoint":                        s.cfg.BaseURL + "/oauth/token",
		"registration_endpoint":                 s.cfg.BaseURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// handleRegister implements dynamic client registration (RFC 7591). Every
// client is registered as a public client (no secret): PKCE protects the
// code exchange instead.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		registrationError(w, "invalid_client_metadata", "request body must be JSON client metadata")
		return
	}
	if len(req.RedirectURIs) == 0 {
		registrationError(w, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			registrationError(w, "invalid_redirect_uri", err.Error())
			return
		}
	}
	if len(req.ClientName) > 200 {
		req.ClientName = req.ClientName[:200]
	}

	c, err := s.registerClient(r.Context(), req.ClientName, req.RedirectURIs)
	if err != nil {
		log.Printf("oauth: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Until the client and redirect URI are known to be good, errors must be
	// shown here rather than redirected (RFC 6749 §4.1.2.1).
	c, err := s.getClient(r.Context(), q.Get("client_id"))
	if errors.Is(err, errNotFound) {
		renderError(w, http.StatusBadRequest, "Unknown client", "This MCP client isn't registered with this server. Try removing and re-adding the server in your MCP client.")
		return
	} else if err != nil {
		log.Printf("oauth: %v", err)
		renderError(w, http.StatusInternalServerError, "Something went wrong", "Please try again.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	if !redirectURIAllowed(c.RedirectURIs, redirectURI) {
		renderError(w, http.StatusBadRequest, "Invalid redirect URI", "The redirect URI doesn't match the ones this MCP client registered.")
		return
	}

	state := q.Get("state")
	fail := func(code, description string) {
		redirectWithError(w, r, redirectURI, state, code, description)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if res := q.Get("resource"); res != "" && !oauthex.MatchesResource([]string{s.cfg.ResourceURL}, res) {
		fail("invalid_target", "unknown resource")
		return
	}

	verifier := oauth2.GenerateVerifier()
	id := s.requests.put(authRequest{
		ClientID:       c.ID,
		ClientName:     c.Name,
		RedirectURI:    redirectURI,
		State:          state,
		CodeChallenge:  q.Get("code_challenge"),
		GitHubVerifier: verifier,
	})
	// Tie the GitHub callback to the browser that started the request.
	http.SetCookie(w, &http.Cookie{
		Name:     requestCookie,
		Value:    id,
		Path:     "/oauth/",
		MaxAge:   int(pendingTTL.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.github.AuthCodeURL(id, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("state")
	cookie, err := r.Cookie(requestCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(id)) != 1 {
		renderError(w, http.StatusBadRequest, "Sign-in expired", "This sign-in wasn't started in this browser, or took too long. Please start again from your MCP client.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: requestCookie, Path: "/oauth/", MaxAge: -1})

	req, ok := s.requests.take(id)
	if !ok {
		renderError(w, http.StatusBadRequest, "Sign-in expired", "This sign-in took too long. Please start again from your MCP client.")
		return
	}
	if e := q.Get("error"); e != "" {
		redirectWithError(w, r, req.RedirectURI, req.State, "access_denied", "GitHub sign-in was cancelled")
		return
	}

	login, githubID, err := s.githubUser(r.Context(), q.Get("code"), req.GitHubVerifier)
	if err != nil {
		log.Printf("oauth: GitHub sign-in failed: %v", err)
		renderError(w, http.StatusBadGateway, "GitHub sign-in failed", "Couldn't confirm your GitHub identity. Please try again.")
		return
	}
	if !s.allowed[normalizeLogin(login)] {
		log.Printf("oauth: refused sign-in by GitHub user %q (not in GITHUB_ALLOWED_USERS)", login)
		renderError(w, http.StatusForbidden, "Not allowed", fmt.Sprintf("The GitHub account %q isn't allowed to use this server.", login))
		return
	}

	user, err := s.auth.UpsertGitHubUser(r.Context(), githubID, login)
	if err != nil {
		log.Printf("oauth: %v", err)
		renderError(w, http.StatusInternalServerError, "Something went wrong", "Please try again.")
		return
	}

	consentID := s.consents.put(consent{Request: req, User: *user})
	renderConsent(w, consentPage{
		ConsentID:    consentID,
		ClientName:   req.ClientName,
		RedirectHost: redirectHost(req.RedirectURI),
		Login:        user.Username,
	})
}

func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.consents.take(r.PostFormValue("consent_id"))
	if !ok {
		renderError(w, http.StatusBadRequest, "Request expired", "This request has expired or was already answered. Please start again from your MCP client.")
		return
	}
	if r.PostFormValue("action") != "approve" {
		redirectWithError(w, r, c.Request.RedirectURI, c.Request.State, "access_denied", "the user denied access")
		return
	}

	code := s.codes.put(authCode{
		ClientID:      c.Request.ClientID,
		RedirectURI:   c.Request.RedirectURI,
		CodeChallenge: c.Request.CodeChallenge,
		UserID:        c.User.ID,
	})
	params := url.Values{"code": {code}, "iss": {s.cfg.BaseURL}}
	if c.Request.State != "" {
		params.Set("state", c.Request.State)
	}
	http.Redirect(w, r, appendQuery(c.Request.RedirectURI, params), http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		tokenError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if clientID == "" {
		// Accept HTTP Basic with an empty secret, as some clients send it.
		clientID, _, _ = r.BasicAuth()
	}
	if res := r.PostForm.Get("resource"); res != "" && !oauthex.MatchesResource([]string{s.cfg.ResourceURL}, res) {
		tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}

	var userID int64
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code, ok := s.codes.take(r.PostForm.Get("code"))
		if !ok || code.ClientID != clientID || code.RedirectURI != r.PostForm.Get("redirect_uri") ||
			!pkceMatches(r.PostForm.Get("code_verifier"), code.CodeChallenge) {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid, expired or already used authorization code")
			return
		}
		userID = code.UserID
	case "refresh_token":
		var err error
		userID, err = s.consumeRefreshToken(r.Context(), r.PostForm.Get("refresh_token"), clientID)
		if errors.Is(err, errNotFound) {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
			return
		} else if err != nil {
			log.Printf("oauth: %v", err)
			tokenError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}

	user, err := s.auth.GetUser(r.Context(), userID)
	if err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "account no longer exists")
		return
	}
	accessToken, expiresAt, err := s.auth.IssueAccessToken(user, s.cfg.ResourceURL)
	if err != nil {
		log.Printf("oauth: failed to issue access token: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	refreshToken, err := s.issueRefreshToken(r.Context(), clientID, userID)
	if err != nil {
		log.Printf("oauth: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(expiresAt).Seconds()),
		"refresh_token": refreshToken,
	})
}

// githubUser exchanges a GitHub authorization code and returns who signed in.
func (s *Server) githubUser(ctx context.Context, code, verifier string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token, err := s.github.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", 0, fmt.Errorf("code exchange: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.githubUserURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.github.Client(ctx, token).Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("fetching user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("fetching user: HTTP %d", resp.StatusCode)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", 0, fmt.Errorf("decoding user: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return "", 0, fmt.Errorf("GitHub returned no user id or login")
	}
	return u.Login, u.ID, nil
}

func pkceMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// validateRedirectURI accepts https URLs, http URLs on a loopback address
// (for local clients, RFC 8252 §7.3) and private-use URI schemes (for
// native apps, RFC 8252 §7.1).
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("redirect_uri %q is not an absolute URI", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("redirect_uri %q must not have a fragment", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("redirect_uri %q must use https unless it is a loopback address", raw)
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return fmt.Errorf("redirect_uri scheme %q is not allowed", u.Scheme)
	}
	return nil
}

// redirectURIAllowed reports whether uri is one of the registered redirect
// URIs. For loopback http URIs, any port is accepted (RFC 8252 §7.3), since
// native clients pick a free port at runtime.
func redirectURIAllowed(registered []string, uri string) bool {
	if uri == "" {
		return false
	}
	if slices.Contains(registered, uri) {
		return true
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || !isLoopback(u.Hostname()) {
		return false
	}
	for _, reg := range registered {
		ru, err := url.Parse(reg)
		if err != nil || ru.Scheme != "http" || !isLoopback(ru.Hostname()) {
			continue
		}
		if ru.Hostname() == u.Hostname() && ru.Path == u.Path && ru.RawQuery == u.RawQuery {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func redirectHost(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	if u.Host != "" {
		return u.Host
	}
	return u.Scheme + ":"
}

func redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	params := url.Values{"error": {code}}
	if description != "" {
		params.Set("error_description", description)
	}
	if state != "" {
		params.Set("state", state)
	}
	http.Redirect(w, r, appendQuery(redirectURI, params), http.StatusFound)
}

func appendQuery(uri string, params url.Values) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func resourcePath(resourceURL string) string {
	u, err := url.Parse(resourceURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u.Path, "/")
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// cors allows browser-based MCP clients to call the JSON endpoints.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	writeJSON(w, status, body)
}

func registrationError(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": description})
}
