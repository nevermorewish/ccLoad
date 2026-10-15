package app

import (
	"strings"
	"sync"
	"time"

	"ccLoad/internal/protocol"
	"github.com/tidwall/gjson"
)

// HTTP response IDs belong to one upstream account. This bounded process-local
// directory stores routing identity, never the credential or conversation body.
// Restart/expiry is an explicit failure; it must not silently start a new turn.
const responsesHTTPRouteLimit = 10000
const responsesHTTPRouteTTL = time.Hour

type responsesHTTPRoute struct {
	channelID int64
	baseURL   string
	keyHash   [32]byte
	model     string
	expires   time.Time
}

type responsesHTTPRouteKey struct{ tokenHash, responseID string }
type responsesHTTPRouteStore struct {
	mu      sync.Mutex
	entries map[responsesHTTPRouteKey]responsesHTTPRoute
}

func (s *responsesHTTPRouteStore) get(tokenHash, responseID string) (responsesHTTPRoute, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := responsesHTTPRouteKey{tokenHash, responseID}
	route, ok := s.entries[key]
	if ok && !time.Now().Before(route.expires) {
		delete(s.entries, key)
		return responsesHTTPRoute{}, false
	}
	return route, ok
}

func (s *responsesHTTPRouteStore) put(tokenHash, responseID string, route responsesHTTPRoute) {
	if tokenHash == "" || responseID == "" || len(responseID) > 512 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[responsesHTTPRouteKey]responsesHTTPRoute)
	}
	// gjson may return a substring of a large response. Retain only routing fields.
	key := responsesHTTPRouteKey{tokenHash, strings.Clone(responseID)}
	route.model = strings.Clone(route.model)
	if _, exists := s.entries[key]; !exists && len(s.entries) >= responsesHTTPRouteLimit {
		var oldest responsesHTTPRouteKey
		var oldestTime time.Time
		for k, v := range s.entries {
			if oldestTime.IsZero() || v.expires.Before(oldestTime) {
				oldest, oldestTime = k, v.expires
			}
		}
		delete(s.entries, oldest)
	}
	route.expires = time.Now().Add(responsesHTTPRouteTTL)
	s.entries[key] = route
}

func (s *Server) rememberResponsesHTTPRoute(req *proxyRequestContext, res *fwResult, channelID int64, baseURL, key, actualModel string) {
	if req.clientProtocol != protocol.Codex || req.upstreamProtocol != protocol.Codex ||
		protocol.DetectRequestFamily(req.requestPath) != protocol.RequestFamilyResponses ||
		req.nativeCodexWS != nil || req.routingSession != nil || gjson.GetBytes(req.body, "store").Raw == "false" {
		return
	}
	if res.HasResponsesTurnResult && !res.ResponsesTurnResult.interrupted {
		s.responsesHTTPRoutes.put(req.tokenHash, res.ResponsesTurnResult.completedResponseID, responsesHTTPRoute{
			channelID: channelID, baseURL: baseURL, keyHash: codexWebsocketKeyHash(key), model: actualModel,
		})
	}
}
