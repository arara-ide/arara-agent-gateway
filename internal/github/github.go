package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/secret"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	Provider      = "github"
	stateTTL      = 10 * time.Minute
	refreshMargin = 5 * time.Minute
	maxBody       = 4 << 20
)

var (
	ErrDisabled      = errors.New("github integration is not configured")
	ErrInvalidState  = errors.New("invalid or expired oauth state")
	ErrNotConnected  = errors.New("github account is not connected")
	ErrExpired       = errors.New("github credentials expired or were revoked")
	ErrInvalidRepo   = errors.New("invalid repository name")
	ErrRepoForbidden = errors.New("repository not accessible with this github connection")
	ErrUpstream      = errors.New("github request failed")
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func ParseFullName(full string) (string, string, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || !namePattern.MatchString(owner) || !namePattern.MatchString(name) || owner == "." || owner == ".." || name == "." || name == ".." || strings.HasPrefix(owner, "-") || strings.HasPrefix(name, "-") {
		return "", "", ErrInvalidRepo
	}
	return owner, name, nil
}

type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       string
	WebURL       string
	APIURL       string
}

type Token struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitzero"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
	Login            string    `json:"login"`
	UserID           int64     `json:"user_id"`
}

type Identity struct {
	Login  string
	UserID int64
}

type Service struct {
	cfg   Config
	http  *http.Client
	store *store.Store
	box   *secret.Box
	now   func() time.Time
	mu    sync.Mutex
}

func New(cfg Config, s *store.Store, box *secret.Box, client *http.Client, now func() time.Time) *Service {
	if cfg.WebURL == "" {
		cfg.WebURL = "https://github.com"
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, http: client, store: s, box: box, now: now}
}

func (s *Service) Enabled() bool { return s.cfg.ClientID != "" }

func secretAD(userID string) string { return "connection:" + Provider + ":" + userID }

func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) Connect(ctx context.Context, userID string) (string, time.Time, error) {
	if !s.Enabled() {
		return "", time.Time{}, ErrDisabled
	}
	state := randomToken(32)
	verifier := randomToken(48)
	sealed, err := s.box.Seal([]byte(verifier), "oauth-state:"+userID)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().UTC().Add(stateTTL)
	if err := s.store.EnsureUser(ctx, userID, s.now().UTC()); err != nil {
		return "", time.Time{}, err
	}
	if err := s.store.PutOAuthState(ctx, store.OAuthState{StateHash: hashState(state), UserID: userID, Verifier: sealed, ExpiresAt: expires}); err != nil {
		return "", time.Time{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.cfg.RedirectURL},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"allow_signup":          {"false"},
	}
	if s.cfg.Scopes != "" {
		q.Set("scope", s.cfg.Scopes)
	}
	return s.cfg.WebURL + "/login/oauth/authorize?" + q.Encode(), expires, nil
}

func (s *Service) Complete(ctx context.Context, userID, code, state string) (store.Connection, error) {
	if !s.Enabled() {
		return store.Connection{}, ErrDisabled
	}
	if code == "" || state == "" || len(code) > 512 || len(state) > 512 {
		return store.Connection{}, ErrInvalidState
	}
	st, err := s.store.TakeOAuthState(ctx, hashState(state))
	if errors.Is(err, store.ErrNotFound) {
		return store.Connection{}, ErrInvalidState
	}
	if err != nil {
		return store.Connection{}, err
	}
	if st.UserID != userID || s.now().After(st.ExpiresAt) {
		return store.Connection{}, ErrInvalidState
	}
	verifier, err := s.box.Open(st.Verifier, "oauth-state:"+userID)
	if err != nil {
		return store.Connection{}, ErrInvalidState
	}
	tok, err := s.exchange(ctx, url.Values{
		"client_id":     {s.cfg.ClientID},
		"client_secret": {s.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {s.cfg.RedirectURL},
		"code_verifier": {string(verifier)},
	})
	if err != nil {
		return store.Connection{}, err
	}
	id, err := s.identity(ctx, tok.AccessToken)
	if err != nil {
		return store.Connection{}, err
	}
	tok.Login, tok.UserID = id.Login, id.UserID
	return s.save(ctx, userID, tok)
}

func (s *Service) save(ctx context.Context, userID string, tok Token) (store.Connection, error) {
	raw, err := json.Marshal(tok)
	if err != nil {
		return store.Connection{}, err
	}
	sealed, err := s.box.Seal(raw, secretAD(userID))
	if err != nil {
		return store.Connection{}, err
	}
	c := store.Connection{UserID: userID, Provider: Provider, Status: store.ConnConnected, Method: "oauth", Account: tok.Login, Secret: sealed, UpdatedAt: s.now().UTC()}
	return c, s.store.UpsertConnection(ctx, c)
}

func (s *Service) exchange(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if out.Error != "" || out.AccessToken == "" {
		if out.Error == "bad_verification_code" || out.Error == "bad_refresh_token" || out.Error == "incorrect_client_credentials" {
			return Token{}, ErrInvalidState
		}
		return Token{}, ErrUpstream
	}
	now := s.now().UTC()
	tok := Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken}
	if out.ExpiresIn > 0 {
		tok.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if out.RefreshTokenExpiresIn > 0 {
		tok.RefreshExpiresAt = now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second)
	}
	return tok, nil
}

func (s *Service) api(ctx context.Context, token, method, path string, out any) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.APIURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, resp.Header, ErrExpired
	}
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return resp.StatusCode, resp.Header, nil
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return resp.StatusCode, resp.Header, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
	}
	return resp.StatusCode, resp.Header, nil
}

func (s *Service) identity(ctx context.Context, token string) (Identity, error) {
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	status, _, err := s.api(ctx, token, http.MethodGet, "/user", &u)
	if err != nil {
		return Identity{}, err
	}
	if status != http.StatusOK || u.Login == "" {
		return Identity{}, ErrUpstream
	}
	return Identity{Login: u.Login, UserID: u.ID}, nil
}

func (s *Service) markExpired(ctx context.Context, userID string, c store.Connection) {
	c.Status, c.Error, c.UpdatedAt = store.ConnExpired, "token rejected by github", s.now().UTC()
	_ = s.store.UpsertConnection(ctx, c)
}

func (s *Service) Token(ctx context.Context, userID string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.store.GetConnection(ctx, userID, Provider)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (c.Secret == "" || c.Status == store.ConnDisconnected)) {
		return Token{}, ErrNotConnected
	}
	if err != nil {
		return Token{}, err
	}
	if c.Status == store.ConnExpired {
		return Token{}, ErrExpired
	}
	raw, err := s.box.Open(c.Secret, secretAD(userID))
	if err != nil {
		return Token{}, err
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Token{}, err
	}
	if tok.ExpiresAt.IsZero() || s.now().Add(refreshMargin).Before(tok.ExpiresAt) {
		return tok, nil
	}
	if tok.RefreshToken == "" || (!tok.RefreshExpiresAt.IsZero() && s.now().After(tok.RefreshExpiresAt)) {
		s.markExpired(ctx, userID, c)
		return Token{}, ErrExpired
	}
	next, err := s.exchange(ctx, url.Values{
		"client_id":     {s.cfg.ClientID},
		"client_secret": {s.cfg.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
	})
	if errors.Is(err, ErrInvalidState) {
		s.markExpired(ctx, userID, c)
		return Token{}, ErrExpired
	}
	if err != nil {
		return Token{}, err
	}
	next.Login, next.UserID = tok.Login, tok.UserID
	if _, err := s.save(ctx, userID, next); err != nil {
		return Token{}, err
	}
	return next, nil
}

func (s *Service) Status(ctx context.Context, userID string) (store.Connection, error) {
	c, err := s.store.GetConnection(ctx, userID, Provider)
	if errors.Is(err, store.ErrNotFound) {
		return store.Connection{UserID: userID, Provider: Provider, Status: store.ConnDisconnected}, nil
	}
	if err != nil || c.Status != store.ConnConnected {
		return c, err
	}
	tok, err := s.Token(ctx, userID)
	if errors.Is(err, ErrExpired) {
		return s.store.GetConnection(ctx, userID, Provider)
	}
	if err != nil {
		return c, nil
	}
	if _, err := s.identity(ctx, tok.AccessToken); errors.Is(err, ErrExpired) {
		s.markExpired(ctx, userID, c)
		return s.store.GetConnection(ctx, userID, Provider)
	}
	return s.store.GetConnection(ctx, userID, Provider)
}

type Repo struct {
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	CanPush       bool      `json:"can_push"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Service) Repos(ctx context.Context, userID string, page int) ([]Repo, bool, error) {
	tok, err := s.Token(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	if page < 1 {
		page = 1
	}
	var raw []struct {
		FullName      string    `json:"full_name"`
		Private       bool      `json:"private"`
		DefaultBranch string    `json:"default_branch"`
		UpdatedAt     time.Time `json:"updated_at"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	status, header, err := s.api(ctx, tok.AccessToken, http.MethodGet, "/user/repos?per_page=100&sort=updated&page="+strconv.Itoa(page), &raw)
	if errors.Is(err, ErrExpired) {
		s.expireByUser(ctx, userID)
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, ErrUpstream
	}
	out := make([]Repo, 0, len(raw))
	for _, r := range raw {
		out = append(out, Repo{FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch, CanPush: r.Permissions.Push, UpdatedAt: r.UpdatedAt})
	}
	return out, strings.Contains(header.Get("Link"), `rel="next"`), nil
}

func (s *Service) expireByUser(ctx context.Context, userID string) {
	if c, err := s.store.GetConnection(ctx, userID, Provider); err == nil {
		s.markExpired(ctx, userID, c)
	}
}

func (s *Service) Repo(ctx context.Context, userID, fullName string) (Repo, Token, error) {
	owner, name, err := ParseFullName(fullName)
	if err != nil {
		return Repo{}, Token{}, err
	}
	tok, err := s.Token(ctx, userID)
	if err != nil {
		return Repo{}, Token{}, err
	}
	var raw struct {
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	status, _, err := s.api(ctx, tok.AccessToken, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), &raw)
	if errors.Is(err, ErrExpired) {
		s.expireByUser(ctx, userID)
		return Repo{}, Token{}, err
	}
	if err != nil {
		return Repo{}, Token{}, err
	}
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return Repo{}, Token{}, ErrRepoForbidden
	}
	if status != http.StatusOK {
		return Repo{}, Token{}, ErrUpstream
	}
	return Repo{FullName: raw.FullName, Private: raw.Private, DefaultBranch: raw.DefaultBranch, CanPush: raw.Permissions.Push}, tok, nil
}

func (s *Service) Disconnect(ctx context.Context, userID string) (store.Connection, error) {
	s.mu.Lock()
	c, err := s.store.GetConnection(ctx, userID, Provider)
	s.mu.Unlock()
	if err == nil && c.Secret != "" && s.Enabled() {
		if raw, oerr := s.box.Open(c.Secret, secretAD(userID)); oerr == nil {
			var tok Token
			if json.Unmarshal(raw, &tok) == nil {
				s.revoke(ctx, tok.AccessToken)
			}
		}
	}
	next := store.Connection{UserID: userID, Provider: Provider, Status: store.ConnDisconnected, UpdatedAt: s.now().UTC()}
	return next, s.store.UpsertConnection(ctx, next)
}

func (s *Service) revoke(ctx context.Context, accessToken string) {
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.cfg.APIURL+"/applications/"+url.PathEscape(s.cfg.ClientID)+"/grant", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
