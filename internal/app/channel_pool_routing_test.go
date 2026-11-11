package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ccLoad/internal/model"
)

func kimiRouteRule() ChannelRouteRule {
	return ChannelRouteRule{
		ID: "kimi", Name: "kimi-k3 分流", Enabled: true, Models: []string{"kimi-k3"}, DefaultPool: 0,
		TokenTiers: []ChannelRouteTokenTier{{MaxTokens: 8000, Pool: 1}, {MinTokens: 50000, Pool: 2}},
		Sticky:     channelRouteStickyOff,
	}
}

func useChannelRoutes(t *testing.T, server *Server, rules ...ChannelRouteRule) {
	t.Helper()
	config, err := normalizeChannelRouteConfig(ChannelRouteConfig{Rules: rules})
	if err != nil {
		t.Fatalf("normalizeChannelRouteConfig: %v", err)
	}
	if err := server.commitChannelRouteConfig(context.Background(), config); err != nil {
		t.Fatalf("commitChannelRouteConfig: %v", err)
	}
}

func decidePools(server *Server, modelName string, tokens int, session string) *channelPoolRoute {
	return server.decideChannelPoolRoute(channelPoolRouteInput{
		model: modelName, inputTokens: tokens, inputTokensKnown: true, session: func() string { return session }, now: time.Now(),
	})
}

func TestParseChannelRouteConfigNormalizes(t *testing.T) {
	config, err := parseChannelRouteConfig(`{"rules":[{"name":" kimi ","enabled":true,"models":[" kimi-k3(max) ","KIMI-K3","kimi-*",""],"default_pool":0,
		"token_tiers":[{"max_tokens":8000,"pool":1}],"overflow":[{"pool":1,"max_queue":100,"target_pool":2}]}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(config.Rules) != 1 {
		t.Fatalf("rules=%+v", config.Rules)
	}
	rule := config.Rules[0]
	if !validChannelRouteRuleID(rule.ID) {
		t.Fatalf("generated id %q is invalid", rule.ID)
	}
	if rule.Name != "kimi" || !slices.Equal(rule.Models, []string{"kimi-k3", "kimi-*"}) || rule.Sticky != channelRouteStickyOff {
		t.Fatalf("normalized rule=%+v", rule)
	}

	for _, empty := range []string{"", "{}", "null", `{"rules":[]}`} {
		if config, err := parseChannelRouteConfig(empty); err != nil || len(config.Rules) != 0 || config.Rules == nil {
			t.Fatalf("parse(%q)=%+v err=%v", empty, config, err)
		}
	}

	valid := kimiRouteRule()
	cases := map[string]func(*ChannelRouteRule){
		"empty models":         func(r *ChannelRouteRule) { r.Models = []string{" "} },
		"invalid default pool": func(r *ChannelRouteRule) { r.DefaultPool = 3 },
		"unbounded tier":       func(r *ChannelRouteRule) { r.TokenTiers = []ChannelRouteTokenTier{{Pool: 1}} },
		"inverted tier": func(r *ChannelRouteRule) {
			r.TokenTiers = []ChannelRouteTokenTier{{MinTokens: 9000, MaxTokens: 8000, Pool: 1}}
		},
		"negative tier": func(r *ChannelRouteRule) {
			r.TokenTiers = []ChannelRouteTokenTier{{MinTokens: -1, MaxTokens: 10, Pool: 1}}
		},
		"tier pool":     func(r *ChannelRouteRule) { r.TokenTiers = []ChannelRouteTokenTier{{MaxTokens: 10, Pool: -1}} },
		"self overflow": func(r *ChannelRouteRule) { r.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 1, TargetPool: 1}} },
		"zero queue":    func(r *ChannelRouteRule) { r.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 0, TargetPool: 2}} },
		"duplicate overflow": func(r *ChannelRouteRule) {
			r.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 1, TargetPool: 2}, {Pool: 1, MaxQueue: 2, TargetPool: 0}}
		},
		"invalid sticky": func(r *ChannelRouteRule) { r.Sticky = "forever" },
		"invalid id":     func(r *ChannelRouteRule) { r.ID = "bad id" },
		"control name":   func(r *ChannelRouteRule) { r.Name = "a\nb" },
	}
	for name, mutate := range cases {
		rule := valid
		rule.TokenTiers = slices.Clone(valid.TokenTiers)
		mutate(&rule)
		if _, err := normalizeChannelRouteConfig(ChannelRouteConfig{Rules: []ChannelRouteRule{rule}}); err == nil {
			t.Errorf("%s: normalize accepted invalid rule %+v", name, rule)
		}
	}
	if _, err := normalizeChannelRouteConfig(ChannelRouteConfig{Rules: []ChannelRouteRule{valid, valid}}); err == nil {
		t.Fatal("duplicate rule ids accepted")
	}
	if _, err := parseChannelRouteConfig(`{"rules":[]} {}`); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestChannelRouteSnapshotMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"kimi-k3", "kimi-k3", true},
		{"kimi-k3", "kimi-k3-turbo", false},
		{"kimi-*", "kimi-k3", true},
		{"*-k3", "kimi-k3", true},
		{"k*3*o", "kimi-k3-turbo", true},
		{"a*a", "a", false},
		{"*", "anything", true},
	} {
		if got := matchChannelRoutePattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("match(%q,%q)=%v want %v", tc.pattern, tc.name, got, tc.want)
		}
	}

	disabled := kimiRouteRule()
	disabled.ID, disabled.Enabled = "off", false
	wildcard := ChannelRouteRule{ID: "all-kimi", Enabled: true, Models: []string{"KIMI-*"}, Sticky: channelRouteStickyOff}
	snapshot := newChannelRouteSnapshot(ChannelRouteConfig{Rules: []ChannelRouteRule{disabled, wildcard, kimiRouteRule()}})
	if rule := snapshot.match("Kimi-K3(high)"); rule == nil || rule.ID != "all-kimi" {
		t.Fatalf("first enabled matching rule = %+v, want all-kimi", rule)
	}
	if rule := snapshot.match("glm-5"); rule != nil {
		t.Fatalf("unmatched model got rule %+v", rule)
	}
	if rule := snapshot.match("*"); rule != nil {
		t.Fatalf("wildcard request model matched %+v", rule)
	}
}

func TestDecideChannelPoolRouteTokenTiersAndStrict(t *testing.T) {
	server := newInMemoryServer(t)
	// 未命中规则时只允许主渠道：渠道1/2 必须由规则显式选中才参与选路。
	if route := decidePools(server, "kimi-k3", 100, ""); route.ruleID != "" || !slices.Equal(route.pools, []int{0}) {
		t.Fatalf("without rules route=%+v, want only the main pool", route)
	}
	useChannelRoutes(t, server, kimiRouteRule())

	for _, tc := range []struct {
		tokens int
		pools  []int
		tier   int
		reason string
	}{
		{tokens: 7999, pools: []int{1, 0, 2}, tier: 0, reason: channelRouteReasonTokenTier},
		{tokens: 8000, pools: []int{0, 1, 2}, tier: -1, reason: channelRouteReasonDefault},
		{tokens: 49999, pools: []int{0, 1, 2}, tier: -1, reason: channelRouteReasonDefault},
		{tokens: 50000, pools: []int{2, 0, 1}, tier: 1, reason: channelRouteReasonTokenTier},
	} {
		route := decidePools(server, "kimi-k3", tc.tokens, "")
		if !slices.Equal(route.pools, tc.pools) || route.tier != tc.tier || route.reason != tc.reason || route.inputTokens != tc.tokens {
			t.Errorf("tokens=%d route=%+v, want pools=%v tier=%d reason=%s", tc.tokens, route, tc.pools, tc.tier, tc.reason)
		}
	}
	if route := decidePools(server, "glm-5", 100, ""); route.ruleID != "" || !slices.Equal(route.pools, []int{0}) {
		t.Fatalf("unmatched model route=%+v, want only the main pool", route)
	}

	strict := kimiRouteRule()
	strict.Strict = true
	useChannelRoutes(t, server, strict)
	if route := decidePools(server, "kimi-k3", 100, ""); !slices.Equal(route.pools, []int{1}) || !route.restricted() {
		t.Fatalf("strict short route=%+v, want only pool 1", route)
	}
	if route := decidePools(server, "kimi-k3", 20000, ""); !slices.Equal(route.pools, []int{0}) {
		t.Fatalf("strict default route=%+v, want only main pool", route)
	}
}

func TestDecideChannelPoolRouteOverflow(t *testing.T) {
	server := newInMemoryServer(t)
	rule := kimiRouteRule()
	rule.Strict = true
	rule.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 100, TargetPool: 2}, {Pool: 2, MaxQueue: 5, TargetPool: 0}}
	useChannelRoutes(t, server, rule)

	server.channelPoolQueues.add(channelPoolQueueKey{ruleID: "kimi", pool: 1}, 99)
	if route := decidePools(server, "kimi-k3", 100, ""); !slices.Equal(route.pools, []int{1}) || route.reason != channelRouteReasonTokenTier {
		t.Fatalf("below queue limit route=%+v, want pool 1", route)
	}
	// 未命中规则的进行中请求不计入规则队列。
	server.channelPoolQueues.add(channelPoolQueueKey{pool: 1}, 50)
	server.channelPoolQueues.add(channelPoolQueueKey{ruleID: "kimi", pool: 1}, 1)
	route := decidePools(server, "kimi-k3", 100, "")
	if !slices.Equal(route.pools, []int{2, 1}) || route.reason != channelRouteReasonOverflow {
		t.Fatalf("queue at limit route=%+v, want overflow to pool 2 then pool 1", route)
	}
	// 溢出可以接力；目标组也满时继续溢出，被溢出的组按逆序留作回退。
	server.channelPoolQueues.add(channelPoolQueueKey{ruleID: "kimi", pool: 2}, 5)
	if route := decidePools(server, "kimi-k3", 100, ""); !slices.Equal(route.pools, []int{0, 2, 1}) {
		t.Fatalf("chained overflow route=%+v, want [0 2 1]", route)
	}

	// 环形溢出在回到已访问组时停止。
	cycle := kimiRouteRule()
	cycle.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 1, TargetPool: 2}, {Pool: 2, MaxQueue: 1, TargetPool: 1}}
	useChannelRoutes(t, server, cycle)
	if route := decidePools(server, "kimi-k3", 100, ""); !slices.Equal(route.pools, []int{2, 1, 0}) {
		t.Fatalf("cyclic overflow route=%+v, want [2 1 0]", route)
	}
}

func TestDecideChannelPoolRouteSessionStickiness(t *testing.T) {
	server := newInMemoryServer(t)
	rule := kimiRouteRule()
	rule.Sticky = channelRouteStickyTier
	rule.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 1, TargetPool: 2}}
	useChannelRoutes(t, server, rule)

	first := decidePools(server, "kimi-k3", 100, "session-a")
	if !slices.Equal(first.pools, []int{1, 0, 2}) || first.stickyKey.ruleID != "kimi" {
		t.Fatalf("first route=%+v", first)
	}
	server.bindChannelPoolAffinity(first, 1)
	server.channelPoolQueues.add(channelPoolQueueKey{ruleID: "kimi", pool: 1}, 10)

	// 同档位内会话留在已绑定的组，新会话按队列溢出。
	if route := decidePools(server, "kimi-k3", 200, "session-a"); !slices.Equal(route.pools, []int{1, 0, 2}) || route.reason != channelRouteReasonSticky {
		t.Fatalf("sticky route=%+v, want pool 1 first", route)
	}
	if route := decidePools(server, "kimi-k3", 200, "session-b"); route.pools[0] != 2 || route.reason != channelRouteReasonOverflow {
		t.Fatalf("new session route=%+v, want overflow to pool 2", route)
	}
	// 档位变化后重新分流。
	if route := decidePools(server, "kimi-k3", 60000, "session-a"); route.pools[0] != 2 || route.reason != channelRouteReasonTokenTier {
		t.Fatalf("tier change route=%+v, want token tier pool 2", route)
	}

	// 整会话粘性跨档位保持，并把本次计算出的组排在第二位。
	rule.Sticky = channelRouteStickySession
	useChannelRoutes(t, server, rule)
	if route := decidePools(server, "kimi-k3", 200, "session-a"); route.reason == channelRouteReasonSticky {
		t.Fatalf("changed rule kept stale binding: %+v", route)
	}
	server.bindChannelPoolAffinity(decidePools(server, "kimi-k3", 200, "session-a"), 1)
	if route := decidePools(server, "kimi-k3", 60000, "session-a"); !slices.Equal(route.pools, []int{1, 2, 0}) || route.reason != channelRouteReasonSticky {
		t.Fatalf("session sticky route=%+v, want [1 2 0]", route)
	}

	// 无会话身份不绑定，过期绑定失效。
	if route := decidePools(server, "kimi-k3", 60000, ""); route.stickyKey.ruleID != "" {
		t.Fatalf("route without session has sticky key: %+v", route)
	}
	key := channelPoolAffinityKey{ruleID: "kimi", session: "session-a"}
	server.channelPoolAffinity.cleanupExpired(time.Now().Add(sessionAffinityTTL + time.Second))
	if _, ok := server.channelPoolAffinity.lookup(key, time.Now()); ok {
		t.Fatal("expired binding survived cleanup")
	}
}

func TestChannelPoolRouteReservationFollowsAttempts(t *testing.T) {
	queues := newChannelPoolQueues()
	route := &channelPoolRoute{ruleID: "kimi", pools: []int{1, 0, 2}}
	route.reserve(queues)
	route.reserve(queues)
	if queues.inflight("kimi", 1) != 1 {
		t.Fatalf("reserve counts=%v", queues.snapshot())
	}
	route.moveReservation(0)
	if queues.inflight("kimi", 1) != 0 || queues.inflight("kimi", 0) != 1 {
		t.Fatalf("moved counts=%v", queues.snapshot())
	}
	route.releaseReservation()
	route.releaseReservation()
	if counts := queues.snapshot(); len(counts) != 0 {
		t.Fatalf("released counts=%v", counts)
	}
	var nilRoute *channelPoolRoute
	nilRoute.reserve(queues)
	nilRoute.moveReservation(1)
	nilRoute.releaseReservation()
}

// 执行会话钉住的渠道只在位于首选渠道组时置顶，规则改选其他组后会话随之迁移。
func TestChannelPoolRouteLeadingPoolForPinnedChannel(t *testing.T) {
	cands := []*model.Config{{ID: 1, ChannelPool: 2}, {ID: 2, ChannelPool: 0}}
	route := &channelPoolRoute{pools: []int{2, 0, 1}}
	if !route.inLeadingPool(cands, 1) || route.inLeadingPool(cands, 2) || !route.inLeadingPool(cands, 99) {
		t.Fatal("pinned channel leading-pool check is wrong")
	}
	// 首选组没有候选时，候选中顺位最靠前的组即为首选。
	if !route.inLeadingPool([]*model.Config{{ID: 2, ChannelPool: 0}, {ID: 3, ChannelPool: 1}}, 2) {
		t.Fatal("best available pool must lead when the preferred pool has no candidates")
	}
	var noRoute *channelPoolRoute
	if !noRoute.inLeadingPool(cands, 2) {
		t.Fatal("without a route every pinned channel may lead")
	}
}

func TestEstimateRouteInputTokens(t *testing.T) {
	text := strings.Repeat("hello world ", 400) // 4800 字符 ≈ 1200 Token
	image := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG binary", 4096)))
	anthropic, _ := json.Marshal(map[string]any{
		"model": strings.Repeat("m", 4000), "system": text,
		"metadata": map[string]any{"user_id": strings.Repeat("u", 4000)},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": text},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": image}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan", "signature": strings.Repeat("s", 8000)},
			}},
		},
	})
	got := estimateRouteInputTokens(anthropic)
	want := 2*estimateTextTokens(text) + routeBinaryPayloadTokens
	if got < want || got > want+50 {
		t.Fatalf("anthropic estimate=%d, want about %d", got, want)
	}

	openai, _ := json.Marshal(map[string]any{"model": "kimi-k3", "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + image}},
		}},
	}})
	if got := estimateRouteInputTokens(openai); got < estimateTextTokens(text)+routeBinaryPayloadTokens || got > estimateTextTokens(text)+routeBinaryPayloadTokens+50 {
		t.Fatalf("openai estimate=%d", got)
	}

	chinese := strings.Repeat("渠道路由按输入长度分流", 300)
	gemini, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": chinese}}}}})
	if got := estimateRouteInputTokens(gemini); got < estimateTextTokens(chinese) || got > estimateTextTokens(chinese)+10 {
		t.Fatalf("gemini estimate=%d want≈%d", got, estimateTextTokens(chinese))
	}

	if got := estimateRouteInputTokens([]byte("--boundary\r\nnot json")); got != 0 {
		t.Fatalf("non-JSON estimate=%d", got)
	}
	// 普通长文本（含空白/标点）不会被误判为二进制载荷。
	if looksLikeBinaryPayload(text) || !looksLikeBinaryPayload(image) || looksLikeBinaryPayload(`data: {"event": 1}`) {
		t.Fatal("binary payload detection is wrong")
	}
}

func TestEstimateTextTokensMatchesRuneSemantics(t *testing.T) {
	for _, text := range []string{"", "a", "hello", "中文", "混合 mixed 文本", strings.Repeat("长", 600) + strings.Repeat("x", 600), "\xff\xfe broken utf8"} {
		runes := []rune(text)
		want := 0
		if len(runes) > 0 {
			sample := min(len(runes), 500)
			chinese := 0
			for _, r := range runes[:sample] {
				if r >= 0x4E00 && r <= 0x9FFF {
					chinese++
				}
			}
			ratio := float64(chinese) / float64(sample)
			want = max(int(float64(len(runes))/(4.0-(4.0-1.5)*ratio)), 1)
		}
		if got := estimateTextTokens(text); got != want {
			t.Errorf("estimateTextTokens(%q)=%d want %d", text, got, want)
		}
	}
}

func addPoolChannel(t *testing.T, server *Server, name string, pool, priority int, modelName string) *model.Config {
	t.Helper()
	return addPreviewChannel(t, server, &model.Config{
		Name: name, ChannelPool: pool, Priority: priority,
		ModelEntries: []model.ModelEntry{{Model: modelName}},
	}, &model.APIKey{})
}

func candidateNames(cands []*model.Config) []string {
	names := make([]string, len(cands))
	for i, cfg := range cands {
		names[i] = cfg.Name
	}
	return names
}

func TestSelectCandidatesHonorsChannelPoolRoute(t *testing.T) {
	server := newInMemoryServer(t)
	addPoolChannel(t, server, "main-high", 0, 100, "kimi-k3")
	addPoolChannel(t, server, "pool1-low", 1, 1, "kimi-k3")
	addPoolChannel(t, server, "pool2", 2, 50, "kimi-k3")
	ctx := context.Background()

	cands, err := server.selectCandidatesByModelAndClientProtocol(ctx, "kimi-k3", "anthropic")
	if err != nil || !slices.Equal(candidateNames(cands), []string{"main-high", "pool2", "pool1-low"}) {
		t.Fatalf("without route candidates=%v err=%v, want priority order", candidateNames(cands), err)
	}

	route := &channelPoolRoute{ruleID: "kimi", pools: []int{1, 0, 2}}
	cands, err = server.selectCandidatesByModelAndClientProtocol(withChannelPoolRoute(ctx, route), "kimi-k3", "anthropic")
	if err != nil || !slices.Equal(candidateNames(cands), []string{"pool1-low", "main-high", "pool2"}) {
		t.Fatalf("routed candidates=%v err=%v, want pool order before priority", candidateNames(cands), err)
	}

	strict := &channelPoolRoute{ruleID: "kimi", pools: []int{2}}
	cands, err = server.selectCandidatesByModelAndClientProtocol(withChannelPoolRoute(ctx, strict), "kimi-k3", "anthropic")
	if err != nil || !slices.Equal(candidateNames(cands), []string{"pool2"}) {
		t.Fatalf("strict candidates=%v err=%v", candidateNames(cands), err)
	}

	// 健康度排序路径同样以渠道组顺位为第一排序键。
	server.healthCache = &HealthCache{config: model.HealthScoreConfig{Enabled: true}}
	cands, err = server.selectCandidatesByModelAndClientProtocol(withChannelPoolRoute(ctx, route), "kimi-k3", "anthropic")
	if err != nil || !slices.Equal(candidateNames(cands), []string{"pool1-low", "main-high", "pool2"}) {
		t.Fatalf("health-sorted candidates=%v err=%v", candidateNames(cands), err)
	}
}

func TestPreferSessionAffinityChannelRespectsPoolRank(t *testing.T) {
	server := newInMemoryServer(t)
	cands := []*model.Config{
		{ID: 1, Name: "pool1-a", ChannelPool: 1, Priority: 10},
		{ID: 2, Name: "pool1-b", ChannelPool: 1, Priority: 10},
		{ID: 3, Name: "main", ChannelPool: 0, Priority: 50},
	}
	route := &channelPoolRoute{pools: []int{1, 0, 2}}
	if got := server.preferSessionAffinityChannel(slices.Clone(cands), 2, route); got[0].ID != 2 {
		t.Fatalf("bound channel in preferred pool not promoted: %v", candidateNames(got))
	}
	if got := server.preferSessionAffinityChannel(slices.Clone(cands), 3, route); got[0].ID != 1 {
		t.Fatalf("bound channel in fallback pool was promoted: %v", candidateNames(got))
	}
	if got := server.preferSessionAffinityChannel(slices.Clone(cands), 1, nil); got[0].ID != 1 {
		t.Fatalf("without route bound lower-priority channel must stay: %v", candidateNames(got))
	}
}

// ============================================================================
// HTTP 代理端到端
// ============================================================================

type poolProxyEnv struct {
	env    *proxyTestEnv
	served chan string
	fail   *sync.Map
}

func newPoolProxyEnv(t *testing.T) *poolProxyEnv {
	t.Helper()
	names := []string{"main", "pool1", "pool2"}
	served := make(chan string, 64)
	fail := &sync.Map{}
	upstreams := make(map[int]string, len(names))
	channels := make([]testChannel, 0, len(names))
	for index, name := range names {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if broken, _ := fail.Load(name); broken == true {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
				return
			}
			served <- name
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"kimi-k3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		t.Cleanup(upstream.Close)
		upstreams[index] = upstream.URL
		channels = append(channels, testChannel{name: name, upstreamProtocol: "anthropic", models: "kimi-k3", priority: 100, channelPool: index})
	}
	return &poolProxyEnv{env: setupProxyTestEnv(t, channels, upstreams), served: served, fail: fail}
}

func (p *poolProxyEnv) send(t *testing.T, text, session string) (int, string) {
	t.Helper()
	headers := map[string]string{}
	if session != "" {
		headers["X-Claude-Code-Session-Id"] = session
	}
	response := doProxyRequest(t, p.env.engine, "/v1/messages", map[string]any{
		"model": "kimi-k3", "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": text}},
	}, headers)
	if response.Code != http.StatusOK {
		return response.Code, response.Body.String()
	}
	select {
	case name := <-p.served:
		return response.Code, name
	default:
		t.Fatal("request succeeded without reaching an upstream")
		return 0, ""
	}
}

// 约 2 万与 6 万 Token 的英文正文（4 字符/Token）。
var (
	mediumPrompt = strings.Repeat("word ", 16000)
	largePrompt  = strings.Repeat("word ", 48000)
)

func TestProxy_ChannelRouteSplitsByInputTokens(t *testing.T) {
	t.Parallel()
	p := newPoolProxyEnv(t)
	useChannelRoutes(t, p.env.server, kimiRouteRule())

	for _, tc := range []struct{ text, want string }{
		{"hi", "pool1"}, {mediumPrompt, "main"}, {largePrompt, "pool2"}, {"hello again", "pool1"},
	} {
		if status, got := p.send(t, tc.text, ""); status != http.StatusOK || got != tc.want {
			t.Fatalf("prompt of %d bytes served by %s (status %d), want %s", len(tc.text), got, status, tc.want)
		}
	}
	if counts := p.env.server.channelPoolQueues.snapshot(); len(counts) != 0 {
		t.Fatalf("queue reservations leaked: %v", counts)
	}

	// 非严格模式：目标组失败后回退到其他组。
	p.fail.Store("pool1", true)
	if status, got := p.send(t, "hi", ""); status != http.StatusOK || got != "main" {
		t.Fatalf("fallback served by %s (status %d), want main", got, status)
	}
}

func TestProxy_ChannelRouteStrictDoesNotFallBack(t *testing.T) {
	t.Parallel()
	p := newPoolProxyEnv(t)
	rule := kimiRouteRule()
	rule.Strict = true
	useChannelRoutes(t, p.env.server, rule)
	p.fail.Store("pool1", true)

	// 目标组失败（含随后的全冷却兜底）都不回退到其他组。
	for range 2 {
		if status, body := p.send(t, "hi", ""); status == http.StatusOK {
			t.Fatalf("strict route fell back to %s", body)
		}
	}
	select {
	case name := <-p.served:
		t.Fatalf("strict route reached %s", name)
	default:
	}

	// 目标组没有可用渠道时直接报错，并说明渠道路由的限制。
	configs, err := p.env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.Name == "pool1" {
			if _, err := p.env.store.UpdateChannelEnabled(context.Background(), cfg.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	p.env.server.InvalidateChannelListCache()
	status, body := p.send(t, "hi", "")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "channel route") {
		t.Fatalf("status=%d body=%s, want 503 with channel route hint", status, body)
	}
	if status, got := p.send(t, mediumPrompt, ""); status != http.StatusOK || got != "main" {
		t.Fatalf("default tier served by %s (status %d), want main", got, status)
	}
}

func TestProxy_ChannelRouteQueueOverflowAndStickiness(t *testing.T) {
	t.Parallel()
	p := newPoolProxyEnv(t)
	rule := kimiRouteRule()
	rule.Sticky = channelRouteStickyTier
	rule.Overflow = []ChannelRouteOverflow{{Pool: 1, MaxQueue: 3, TargetPool: 2}}
	useChannelRoutes(t, p.env.server, rule)
	ruleID := p.env.server.channelRouteConfig().Rules[0].ID
	queues := p.env.server.channelPoolQueues
	// Claude Code 的会话 ID 是 UUID。
	sessionA, sessionB := uuid.NewString(), uuid.NewString()

	if status, got := p.send(t, "hi", sessionA); status != http.StatusOK || got != "pool1" {
		t.Fatalf("first session request served by %s (status %d), want pool1", got, status)
	}

	// 渠道1 的 kimi 队列达到上限：新会话溢出到渠道2，已绑定会话保持在渠道1。
	queues.add(channelPoolQueueKey{ruleID: ruleID, pool: 1}, 3)
	if status, got := p.send(t, "hi", sessionB); status != http.StatusOK || got != "pool2" {
		t.Fatalf("overflowed request served by %s (status %d), want pool2", got, status)
	}
	if status, got := p.send(t, "hi", sessionA); status != http.StatusOK || got != "pool1" {
		t.Fatalf("sticky session served by %s (status %d), want pool1", got, status)
	}
	if status, got := p.send(t, "hi", sessionB); status != http.StatusOK || got != "pool2" {
		t.Fatalf("overflowed session served by %s (status %d), want to stay on pool2", got, status)
	}
	// 队列回落后无会话请求回到渠道1；档位变化的会话按 Token 重新分流。
	queues.add(channelPoolQueueKey{ruleID: ruleID, pool: 1}, -3)
	if status, got := p.send(t, "hi", ""); status != http.StatusOK || got != "pool1" {
		t.Fatalf("drained queue served by %s (status %d), want pool1", got, status)
	}
	if status, got := p.send(t, mediumPrompt, sessionA); status != http.StatusOK || got != "main" {
		t.Fatalf("tier change served by %s (status %d), want main", got, status)
	}
	if counts := queues.snapshot(); len(counts) != 0 {
		t.Fatalf("queue reservations leaked: %v", counts)
	}
}

func TestProxy_ChannelRouteRequestedChannelHeaderBypassesPools(t *testing.T) {
	t.Parallel()
	p := newPoolProxyEnv(t)
	rule := kimiRouteRule()
	rule.Strict = true
	useChannelRoutes(t, p.env.server, rule)

	response := doProxyRequest(t, p.env.engine, "/v1/messages", map[string]any{
		"model": "kimi-k3", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{"x-ccload-channel": "pool2"})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := <-p.served; got != "pool2" {
		t.Fatalf("explicit channel served by %s", got)
	}
}

// Responses WebSocket 每个 turn 按完整 transcript 重新分流，turn 结束后归还队列名额。
func TestResponsesWebsocketTurnsFollowChannelRoute(t *testing.T) {
	t.Parallel()
	served := make(chan string, 8)
	upstreams := make(map[int]string, 3)
	channels := make([]testChannel, 0, 3)
	for index, name := range []string{"main", "pool1", "pool2"} {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served <- name
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-"+name+"\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		}))
		upstreams[index] = upstream.URL
		channels = append(channels, testChannel{name: name, upstreamProtocol: "codex", models: "gpt-test", priority: 100, channelPool: index})
	}
	env := setupProxyTestEnv(t, channels, upstreams)
	rule := kimiRouteRule()
	rule.Models = []string{"gpt-test"}
	useChannelRoutes(t, env.server, rule)

	for _, tc := range []struct{ text, want string }{{"hello", "pool1"}, {largePrompt, "pool2"}} {
		conn := dialResponsesWebsocket(t, env.engine)
		if err := conn.WriteJSON(map[string]any{
			"type": "response.create", "model": "gpt-test",
			"input": []any{map[string]any{"role": "user", "content": tc.text}},
		}); err != nil {
			t.Fatalf("write turn: %v", err)
		}
		readWebsocketUntilType(t, conn, "response.completed")
		if got := <-served; got != tc.want {
			t.Fatalf("turn of %d bytes served by %s, want %s", len(tc.text), got, tc.want)
		}
		_ = conn.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(env.server.channelPoolQueues.snapshot()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("websocket turn reservations leaked: %v", env.server.channelPoolQueues.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
