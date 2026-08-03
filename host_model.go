package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func executeHostModel(_ context.Context, request pluginapi.HostModelExecutionRequest, callbackID string) (pluginapi.HostModelExecutionResponse, error) {
	result, err := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: request,
		HostCallbackID:            callbackID,
	})
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return pluginapi.HostModelExecutionResponse{}, errors.New("host model execution returned an invalid response")
	}
	return response, nil
}
