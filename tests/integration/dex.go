//go:build integration

// Dex login helper — port Go của sources/opsense-rs crates/opsense/tests/common/dex.rs
// (và scripts/mint-dex-token.py): drive HTML login của Dex để đổi ra id_token RS256.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	dexClientID     = "ltp-test"
	dexClientSecret = "ltp-dev-shared-secret-32-bytes-min!!"
	dexUserEmail    = "dev-user@example.com"
	dexUserPassword = "password"
	dexRedirectURI  = "http://localhost:8080/callback" // gateway /login dựng redirect_uri http://<host>/callback (dev HTTP)
)

type dexEndpoints struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
}

func dexDiscovery(ctx context.Context) (dexEndpoints, error) {
	var ep dexEndpoints
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dexIssuer()+"/.well-known/openid-configuration", nil)
	if err != nil {
		return ep, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ep, fmt.Errorf("dex discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ep, fmt.Errorf("dex discovery: status %d", resp.StatusCode)
	}
	return ep, json.NewDecoder(resp.Body).Decode(&ep)
}

var (
	formActionRe = regexp.MustCompile(`(?is)<form[^>]*action="([^"]+)"`)
	inputTagRe   = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	attrRe       = func(attr string) *regexp.Regexp {
		return regexp.MustCompile(`(?is)<input\b[^>]*\b` + attr + `="([^"]*)"`)
	}
	inputNameRe  = attrRe("name")
	inputValueRe = attrRe("value")
	inputTypeRe  = attrRe("type")
)

// parseForm lấy form đầu tiên trong HTML: action + hidden inputs (+ fill
// credentials nếu là login form).
func parseForm(html string) *dexForm {
	m := formActionRe.FindStringSubmatch(html)
	if m == nil {
		return nil
	}
	f := &dexForm{actionURL: strings.ReplaceAll(m[1], "&amp;", "&"), fields: url.Values{}}
	for _, tag := range inputTagRe.FindAllString(html, -1) {
		if t := inputTypeRe.FindStringSubmatch(tag); t == nil || t[1] != "hidden" {
			continue
		}
		n := inputNameRe.FindStringSubmatch(tag)
		v := inputValueRe.FindStringSubmatch(tag)
		if n == nil {
			continue
		}
		val := ""
		if v != nil {
			val = v[1]
		}
		f.fields.Set(n[1], val)
	}
	// Login form: điền credentials (không đụng form approval đã có hidden inputs).
	if strings.Contains(html, "password") && f.fields.Get("login") == "" && f.fields.Get("username") == "" {
		f.fields.Set("login", dexUserEmail)
		f.fields.Set("password", dexUserPassword)
	}
	return f
}

// dexLoginGetIDToken drive toàn bộ HTML login flow của Dex và trả về id_token.
func dexLoginGetIDToken(ctx context.Context) (string, error) {
	ep, err := dexDiscovery(ctx)
	if err != nil {
		return "", err
	}

	// B1: GET authorization endpoint — KHÔNG follow redirect; Location cuối cùng
	// (sau các form của Dex) chứa ?code= và trỏ về gateway callback.
	authz := fmt.Sprintf("%s?response_type=code&client_id=%s&redirect_uri=%s&scope=%s",
		ep.Authorization,
		url.QueryEscape(dexClientID),
		url.QueryEscape(dexRedirectURI),
		url.QueryEscape("openid email profile offline_access"))

	current := authz
	var pendingForm *dexForm
	var code string
	httpc := &http.Client{Timeout: 30 * time.Second}

	for i := 0; i < 10; i++ {
		var resp *http.Response
		var err error
		if pendingForm != nil {
			// Bước form: POST login/approve với hidden inputs (+ credentials nếu login form)
			data := url.Values(pendingForm.fields)
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, pendingForm.action(current), strings.NewReader(data.Encode()))
			if err != nil {
				return "", err
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err = httpc.Do(req)
		} else {
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
			if err != nil {
				return "", err
			}
			resp, err = httpc.Do(req)
		}
		if err != nil {
			return "", fmt.Errorf("dex flow: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			u, err := url.Parse(loc)
			if err != nil {
				return "", err
			}
			if u.Query().Get("code") != "" {
				code = u.Query().Get("code")
				break
			}
			// Relative redirect → resolve; về lại chế độ GET
			base, _ := url.Parse(current)
			current = base.ResolveReference(u).String()
			pendingForm = nil
			continue
		}

		// HTML form (login / approval) → POST ở vòng kế tiếp
		pendingForm = parseForm(string(body))
		if pendingForm == nil {
			return "", fmt.Errorf("dex flow: unexpected page at %s (status %d)", current, resp.StatusCode)
		}
	}
	if code == "" {
		return "", fmt.Errorf("dex flow: no authorization code captured")
	}

	// B2: exchange code → token
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", dexRedirectURI)
	form.Set("client_id", dexClientID)
	form.Set("client_secret", dexClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("dex token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("dex token exchange: status %d body %.200s", resp.StatusCode, raw)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", err
	}
	if tok.IDToken == "" {
		return "", fmt.Errorf("dex token exchange: empty id_token")
	}
	return tok.IDToken, nil
}

type dexForm struct {
	actionURL string
	fields    url.Values
}

func (f dexForm) action(current string) string {
	base, err := url.Parse(current)
	if err != nil {
		return f.actionURL
	}
	ref, err := url.Parse(f.actionURL)
	if err != nil {
		return f.actionURL
	}
	return base.ResolveReference(ref).String()
}
