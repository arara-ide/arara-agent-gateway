package github_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

type fakeGitHub struct {
	mu         sync.Mutex
	challenges map[string]string
	tokens     map[string]string
	revoked    []string
	expiresIn  int
	refreshes  int
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Form.Get("client_secret") != "secret" {
			json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		resp := map[string]any{"token_type": "bearer"}
		if r.Form.Get("grant_type") == "refresh_token" {
			f.refreshes++
			resp["access_token"] = "gho_refreshed"
			resp["refresh_token"] = "ghr_2"
			f.tokens["gho_refreshed"] = "octo"
		} else {
			code := r.Form.Get("code")
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if f.challenges[code] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
				return
			}
			tok := "gho_" + code
			f.tokens[tok] = "octo"
			resp["access_token"] = tok
			if f.expiresIn > 0 {
				resp["expires_in"] = f.expiresIn
				resp["refresh_token"] = "ghr_1"
				resp["refresh_token_expires_in"] = 3600 * 24
			}
		}
		json.NewEncoder(w).Encode(resp)
	})
	auth := func(r *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		return ok
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "octo", "id": 42})
	})
	mux.HandleFunc("GET /user/repos", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Link", `<x>; rel="next"`)
		json.NewEncoder(w).Encode([]map[string]any{{"full_name": "octo/app", "private": true, "default_branch": "main", "permissions": map[string]bool{"push": true}}})
	})
	mux.HandleFunc("GET /repos/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		if r.PathValue("owner") != "octo" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"full_name": "octo/" + r.PathValue("name"), "default_branch": "main", "permissions": map[string]bool{"push": false}})
	})
	mux.HandleFunc("DELETE /applications/{id}/grant", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AccessToken string `json:"access_token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.revoked = append(f.revoked, body.AccessToken)
		delete(f.tokens, body.AccessToken)
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) (*github.Service, *fakeGitHub, *testutil.Env) {
	env := testutil.NewEnv(t, 10)
	f := &fakeGitHub{challenges: map[string]string{}, tokens: map[string]string{}}
	srv := f.server(t)
	gh := github.New(github.Config{ClientID: "cid", ClientSecret: "secret", RedirectURL: "https://acweb.example/github/callback", Scopes: "repo", WebURL: srv.URL, APIURL: srv.URL}, env.Store, env.Box, srv.Client(), env.Clock.Now)
	return gh, f, env
}

func makeInst(t *testing.T, env *testutil.Env, name string) string {
	t.Helper()
	in, err := env.Instances.Create(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return in.ID
}

func authorize(t *testing.T, gh *github.Service, f *fakeGitHub, instanceID, code string) string {
	t.Helper()
	raw, _, err := gh.Connect(context.Background(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "https://acweb.example/github/callback" || q.Get("scope") != "repo" || len(q.Get("state")) < 40 {
		t.Fatalf("authorize url %s", raw)
	}
	f.mu.Lock()
	f.challenges[code] = q.Get("code_challenge")
	f.mu.Unlock()
	return q.Get("state")
}

func TestConnectFlowStoresEncryptedToken(t *testing.T) {
	gh, f, env := setup(t)
	id := makeInst(t, env, "one")
	state := authorize(t, gh, f, id, "abc")
	c, err := gh.Complete(context.Background(), id, "abc", state)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != store.ConnConnected || c.Account != "octo" {
		t.Fatalf("connection %+v", c)
	}
	stored, _ := env.Store.GetConnection(context.Background(), id, github.Provider)
	if strings.Contains(stored.Secret, "gho_abc") || !strings.HasPrefix(stored.Secret, "v1:") {
		t.Fatal("token not encrypted at rest")
	}
	if _, err := gh.Complete(context.Background(), id, "abc", state); !errors.Is(err, github.ErrInvalidState) {
		t.Fatal("state reused")
	}
}

func TestStateBoundToInstance(t *testing.T) {
	gh, f, env := setup(t)
	a := makeInst(t, env, "a")
	b := makeInst(t, env, "b")
	state := authorize(t, gh, f, a, "evil")
	if _, err := gh.Complete(context.Background(), b, "evil", state); !errors.Is(err, github.ErrInvalidState) {
		t.Fatalf("cross-instance completion accepted: %v", err)
	}
	if _, err := env.Store.GetConnection(context.Background(), b, github.Provider); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("other instance got a connection")
	}
}

func TestStateExpires(t *testing.T) {
	gh, f, env := setup(t)
	id := makeInst(t, env, "one")
	state := authorize(t, gh, f, id, "abc")
	env.Clock.Advance(11 * time.Minute)
	if _, err := gh.Complete(context.Background(), id, "abc", state); !errors.Is(err, github.ErrInvalidState) {
		t.Fatal("expired state accepted")
	}
}

func TestReposAndCrossInstanceIsolation(t *testing.T) {
	gh, f, env := setup(t)
	a := makeInst(t, env, "a")
	b := makeInst(t, env, "b")
	state := authorize(t, gh, f, a, "abc")
	if _, err := gh.Complete(context.Background(), a, "abc", state); err != nil {
		t.Fatal(err)
	}
	repos, more, err := gh.Repos(context.Background(), a, 1)
	if err != nil || len(repos) != 1 || repos[0].FullName != "octo/app" || !repos[0].CanPush || !more {
		t.Fatalf("repos %+v %v %v", repos, more, err)
	}
	if _, _, err := gh.Repos(context.Background(), b, 1); !errors.Is(err, github.ErrNotConnected) {
		t.Fatalf("instance b accessed instance a repos: %v", err)
	}
	if _, _, err := gh.Repo(context.Background(), a, "other/app"); !errors.Is(err, github.ErrRepoForbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

func TestRevokedTokenIsDetected(t *testing.T) {
	gh, f, env := setup(t)
	id := makeInst(t, env, "one")
	state := authorize(t, gh, f, id, "abc")
	gh.Complete(context.Background(), id, "abc", state)
	f.mu.Lock()
	delete(f.tokens, "gho_abc")
	f.mu.Unlock()
	if _, _, err := gh.Repos(context.Background(), id, 1); !errors.Is(err, github.ErrExpired) {
		t.Fatalf("expected expired, got %v", err)
	}
	c, _ := env.Store.GetConnection(context.Background(), id, github.Provider)
	if c.Status != store.ConnExpired {
		t.Fatalf("status %s", c.Status)
	}
	st, _ := gh.Status(context.Background(), id)
	if st.Status != store.ConnExpired {
		t.Fatalf("status endpoint %s", st.Status)
	}
}

func TestRefreshExpiringToken(t *testing.T) {
	gh, f, env := setup(t)
	id := makeInst(t, env, "one")
	f.expiresIn = 600
	state := authorize(t, gh, f, id, "abc")
	gh.Complete(context.Background(), id, "abc", state)
	env.Clock.Advance(8 * time.Minute)
	tok, err := gh.Token(context.Background(), id)
	if err != nil || tok.AccessToken != "gho_refreshed" || f.refreshes != 1 {
		t.Fatalf("refresh: %v %v %d", tok.AccessToken, err, f.refreshes)
	}
}

func TestDisconnectRevokesAndForgets(t *testing.T) {
	gh, f, env := setup(t)
	id := makeInst(t, env, "one")
	state := authorize(t, gh, f, id, "abc")
	gh.Complete(context.Background(), id, "abc", state)
	c, err := gh.Disconnect(context.Background(), id)
	if err != nil || c.Status != store.ConnDisconnected {
		t.Fatalf("disconnect %v", err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != "gho_abc" {
		t.Fatalf("revoked %v", f.revoked)
	}
	stored, _ := env.Store.GetConnection(context.Background(), id, github.Provider)
	if stored.Secret != "" {
		t.Fatal("secret kept after disconnect")
	}
	if _, err := gh.Token(context.Background(), id); !errors.Is(err, github.ErrNotConnected) {
		t.Fatal("token still usable")
	}
}

func TestParseFullName(t *testing.T) {
	for _, ok := range []string{"octo/app", "a-b/c.d_e"} {
		if _, _, err := github.ParseFullName(ok); err != nil {
			t.Fatalf("%s rejected", ok)
		}
	}
	for _, bad := range []string{"", "octo", "../x", "octo/..", "-x/y", "a/b/c", "a b/c", "octo/app;rm"} {
		if _, _, err := github.ParseFullName(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestDisabled(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	id := makeInst(t, env, "one")
	gh := github.New(github.Config{}, env.Store, env.Box, nil, env.Clock.Now)
	if _, _, err := gh.Connect(context.Background(), id); !errors.Is(err, github.ErrDisabled) {
		t.Fatal("expected disabled")
	}
}
