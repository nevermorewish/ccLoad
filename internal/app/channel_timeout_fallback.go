package app

import (
	"context"
	"fmt"
	"net/http"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

type terminalChannelAttemptKey struct{}

func terminalChannelAttempt(ctx context.Context) bool {
	terminal, _ := ctx.Value(terminalChannelAttemptKey{}).(bool)
	return terminal
}

func shouldUseTimeoutChannel(req *proxyRequestContext, result *proxyResult) bool {
	return !req.terminalChannel && req.channelPoolRoute != nil &&
		req.channelPoolRoute.firstByteTimeoutChannelID > 0 && result != nil &&
		result.status == util.StatusFirstByteTimeout && !result.succeeded &&
		!result.isClientCanceled && result.nextAction != cooldown.ActionReturnClient
}

// tryTimeoutChannel performs the rule's final, explicitly addressed attempt.
// The destination may be outside the normal pool order, but never outside the
// token scope, model capabilities, availability window or configured limits.
func (s *Server) tryTimeoutChannel(ctx context.Context, sourceID int64, req *proxyRequestContext, w http.ResponseWriter) (*proxyResult, bool) {
	targetID := req.channelPoolRoute.firstByteTimeoutChannelID
	fail := func(status int, message string) (*proxyResult, bool) {
		return &proxyResult{status: status, channelID: &targetID, body: []byte(message), nextAction: cooldown.ActionReturnClient}, false
	}
	if ctx.Err() != nil {
		return buildCtxDoneResult(&model.Config{ID: targetID}, ctx.Err()), false
	}
	if targetID == sourceID {
		return fail(http.StatusBadGateway, "first byte timeout target is the timed-out channel")
	}
	cfg, err := s.store.GetConfig(ctx, targetID)
	if err != nil || cfg == nil || !cfg.Enabled || cfg.IsSystemOne() || !s.configSupportsModelWithFuzzyMatch(cfg, req.originalModel) {
		return fail(http.StatusServiceUnavailable, fmt.Sprintf("first byte timeout target channel %d is unavailable for this model", targetID))
	}
	// Use a fresh pool decision only for destination eligibility. Preserve the
	// original reservation so the in-flight count follows the final attempt.
	ctx = context.WithValue(ctx, terminalChannelAttemptKey{}, true)
	ctx = withChannelPoolRoute(ctx, &channelPoolRoute{pools: []int{cfg.ChannelPool}})
	eligible, err := s.filterCooldownChannels(ctx, []*model.Config{cfg}, req.originalModel, string(req.clientProtocol))
	if err != nil || len(eligible) == 0 {
		return fail(http.StatusServiceUnavailable, fmt.Sprintf("first byte timeout target channel %d is not eligible", targetID))
	}
	req.terminalChannel = true
	defer func() { req.terminalChannel = false }()
	req.channelPoolRoute.moveReservation(cfg.ChannelPool)
	result, err := s.tryChannelWithSameChannelRetries(ctx, eligible[0], req, w)
	if err != nil {
		return fail(http.StatusBadGateway, fmt.Sprintf("first byte timeout target channel %d could not be used", targetID))
	}
	if result == nil {
		return fail(http.StatusBadGateway, "first byte timeout target returned no result")
	}
	result.nextAction = cooldown.ActionReturnClient
	if !result.succeeded && result.header != nil && result.status >= 300 && result.status < 596 {
		// Preserve the destination HTTP error after the usual output-header filtering.
		writeResponseWithHeaders(w, result.status, result.header, result.body)
		return result, true
	}
	return result, result.succeeded
}
