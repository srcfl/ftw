package energyforecast

import (
	"context"
	"errors"
)

func (c *Client) Reset(ctx context.Context, request ResetRequest) (ResetReply, error) {
	var reply ResetReply
	if err := validateContext(request.RequestContext); err != nil {
		return reply, err
	}
	if (request.Signal != "pv" && request.Signal != "load") ||
		(request.Signal == "pv" && request.Config.PV == nil) ||
		(request.Signal == "load" && request.Config.Load == nil) {
		return reply, errors.New("forecast reset needs an enabled pv or load signal")
	}
	if request.LearningStartedMs <= 0 || request.LearningStartedMs > request.OriginMs {
		return reply, errors.New("forecast learning start must be positive and no later than origin")
	}
	payload := struct {
		Op      string `json:"op"`
		Version int    `json:"version"`
		Action  string `json:"action"`
		ResetRequest
	}{"forecast", ProtocolVersion, "reset", request}
	if err := c.call(ctx, payload, request.RequestContext, "reset", &reply); err != nil {
		return ResetReply{}, err
	}
	if err := validateState(reply.State, true); err != nil {
		return ResetReply{}, err
	}
	if reply.Signal != request.Signal || reply.LearningStartedMs < request.LearningStartedMs || reply.LearningStartedMs > request.OriginMs {
		return ResetReply{}, errors.New("forecast reset reply changed signal or learning start")
	}
	return reply, nil
}
