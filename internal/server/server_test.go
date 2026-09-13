package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"frp-more/internal/logbuf"
)

func TestAuthenticationProtectsManagementAPI(t *testing.T) {
	h := New(nil, logbuf.New(10), AuthConfig{Username: "admin", Password: "admin123"})
	ts := httptest.NewServer(h.Handler)
	defer ts.Close()

	public, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	if public.StatusCode != http.StatusOK {
		t.Fatalf("version status = %d, want %d", public.StatusCode, http.StatusOK)
	}
	public.Body.Close()

	unauth, err := http.Get(ts.URL + "/api/logs")
	if err != nil {
		t.Fatal(err)
	}
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated logs status = %d, want %d", unauth.StatusCode, http.StatusUnauthorized)
	}
	unauth.Body.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	wrong, err := postJSON(client, ts.URL+"/api/login", `{"username":"admin","password":"wrong"}`)
	if err != nil {
		t.Fatal(err)
	}
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want %d", wrong.StatusCode, http.StatusUnauthorized)
	}
	wrong.Body.Close()

	login, err := postJSON(client, ts.URL+"/api/login", `{"username":"admin","password":"admin123"}`)
	if err != nil {
		t.Fatal(err)
	}
	if login.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(login.Body)
		login.Body.Close()
		t.Fatalf("login status = %d, body = %s", login.StatusCode, body)
	}
	login.Body.Close()

	authenticated, err := client.Get(ts.URL + "/api/logs")
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.StatusCode != http.StatusOK {
		t.Fatalf("authenticated logs status = %d, want %d", authenticated.StatusCode, http.StatusOK)
	}
	authenticated.Body.Close()

	logout, err := postJSON(client, ts.URL+"/api/logout", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if logout.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want %d", logout.StatusCode, http.StatusOK)
	}
	logout.Body.Close()

	afterLogout, err := client.Get(ts.URL + "/api/logs")
	if err != nil {
		t.Fatal(err)
	}
	if afterLogout.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout logs status = %d, want %d", afterLogout.StatusCode, http.StatusUnauthorized)
	}
	afterLogout.Body.Close()
}

func postJSON(client *http.Client, url, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func TestSessionResponseDoesNotExposePassword(t *testing.T) {
	h := New(nil, logbuf.New(10), AuthConfig{Username: "admin", Password: "admin123"})
	ts := httptest.NewServer(h.Handler)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login, err := postJSON(client, ts.URL+"/api/login", `{"username":"admin","password":"admin123"}`)
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()

	res, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["password"]; ok {
		t.Fatal("session response contains password")
	}
	if payload["authenticated"] != true {
		t.Fatalf("authenticated = %v, want true", payload["authenticated"])
	}
}
