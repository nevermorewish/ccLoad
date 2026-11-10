package app

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func TestFinalizeCodeBuddyBodyInjectsSystemStreamOptionsAndDeepSeekThinking(t *testing.T) {
	t.Parallel()
	body, err := finalizeCodeBuddyBody([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Fatal("stream not forced")
	}
	if !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
		t.Fatal("stream_options.include_usage missing")
	}
	if gjson.GetBytes(body, "messages.0.role").String() != "system" {
		t.Fatal("system prompt not injected")
	}
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		t.Fatal("deepseek thinking not enabled")
	}
	if gjson.GetBytes(body, "reasoning_effort").String() != "high" {
		t.Fatal("deepseek reasoning_effort missing")
	}
}

func TestFinalizeCodeBuddyBodySkipsThinkingForHy3(t *testing.T) {
	t.Parallel()
	body, err := finalizeCodeBuddyBody([]byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "thinking").Exists() {
		t.Fatal("hy3 must not get deepseek thinking")
	}
}

func TestInjectCodeBuddyHeadersRewritesCodeBuddyIssuerToWorkBuddy(t *testing.T) {
	t.Parallel()
	token := jwtWithIssuer("https://www.codebuddy.ai/auth/realms/copilot")
	credential, err := (&codebuddyauth.Credential{
		AccessToken:  token,
		RefreshToken: "refresh",
		UID:          "uid",
		BaseURL:      codebuddyauth.InternationalBaseURL,
		Domain:       "www.codebuddy.ai",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := injectCodeBuddyHeaders(req, &model.Config{OAuthCredential: credential}, token); err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "www.workbuddy.ai" {
		t.Fatalf("host=%s, want workbuddy.ai", req.URL.Host)
	}
	if req.Header.Get("X-Domain") != "www.workbuddy.ai" || req.Header.Get("Origin") != codebuddyauth.WorkBuddyBaseURL {
		t.Fatalf("headers=%v", req.Header)
	}
}

func TestRewriteCodeBuddyPublicErrorURLsUsesRequestHost(t *testing.T) {
	t.Parallel()
	body := []byte(`visit https://www.codebuddy.ai/profile/usage`)
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := rewriteCodeBuddyPublicErrorURLs(req, body)
	if !strings.Contains(string(got), "https://www.workbuddy.ai/profile/usage") {
		t.Fatalf("got %s", got)
	}
}

func TestInjectCodeBuddyHeadersRewritesWorkBuddyIssuer(t *testing.T) {
	t.Parallel()
	token := jwtWithIssuer("https://www.workbuddy.ai/auth/realms/copilot")
	credential, err := (&codebuddyauth.Credential{
		AccessToken:  token,
		RefreshToken: "refresh",
		UID:          "uid",
		BaseURL:      codebuddyauth.InternationalBaseURL,
		Domain:       "www.codebuddy.ai",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := injectCodeBuddyHeaders(req, &model.Config{OAuthCredential: credential}, token); err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "www.workbuddy.ai" {
		t.Fatalf("host=%s, want workbuddy.ai", req.URL.Host)
	}
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("refresh token leaked onto chat")
	}
	if req.Header.Get("X-Domain") != "www.workbuddy.ai" || req.Header.Get("Origin") != codebuddyauth.WorkBuddyBaseURL {
		t.Fatalf("headers=%v", req.Header)
	}
	if req.Header.Get("X-CodeBuddy-Request") != "1" {
		t.Fatal("missing X-CodeBuddy-Request")
	}
}

func jwtWithIssuer(iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"iss":%q}`, iss))
	return header + "." + payload + ".sig"
}

func TestProxyCodeBuddyResponsesCompactionRequiresRealStop(t *testing.T) {
	for _, ending := range []string{"done-without-stop", "truncated", "length"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"id":"partial","model":"hy3","choices":[{"index":0,"delta":{"content":"partial summary"}}]}`+"\n\n")
				if ending == "length" {
					_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\n")
				}
				if ending != "truncated" {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "codebuddy-compaction-stop", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "hy3", "stream": true,
				"input": []any{map[string]any{"role": "user", "content": "Fix parser"}, map[string]any{"type": "compaction_trigger"}},
			}, nil)
			if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "ccload-compact-v1:") {
				t.Fatalf("unfinished summary reported success: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
