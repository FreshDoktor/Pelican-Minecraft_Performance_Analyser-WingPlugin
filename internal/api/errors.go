package api

import "net/http"

// Error codes of API v1 (docs/API.md, "Errors").
const (
	CodeInvalidRequest       = "invalid_request"
	CodeUnauthorized         = "unauthorized"
	CodeIPNotAllowed         = "ip_not_allowed"
	CodeNotFound             = "not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeServerNotFound       = "server_not_found"
	CodeServerNotRunning     = "server_not_running"
	CodeNoJavaProcess        = "no_java_process"
	CodeHsperfUnavailable    = "hsperf_unavailable"
	CodeAttachUnavailable    = "attach_unavailable"
	CodeAttachFailed         = "attach_failed"
	CodeAttachTimeout        = "attach_timeout"
	CodeOutputTooLarge       = "output_too_large"
	CodeProfileRunning       = "profile_running"
	CodeTooManyProfiles      = "too_many_profiles"
	CodeProfileNotFound      = "profile_not_found"
	CodeInsufficientTmpSpace = "insufficient_tmp_space"
	CodeNotImplemented       = "not_implemented"
	CodeDockerUnavailable    = "docker_unavailable"
	CodeInternal             = "internal_error"
)

// ErrorDef is the HTTP status and default message of an error code.
type ErrorDef struct {
	Code    string
	Status  int
	Message string
}

// Errors lists every error code in documentation order.
var Errors = []ErrorDef{
	{CodeInvalidRequest, http.StatusBadRequest, "The request is invalid."},
	{CodeUnauthorized, http.StatusUnauthorized, "Missing or invalid bearer token."},
	{CodeIPNotAllowed, http.StatusForbidden, "This address is not in allowed_ips."},
	{CodeNotFound, http.StatusNotFound, "No such endpoint."},
	{CodeMethodNotAllowed, http.StatusMethodNotAllowed, "Method not allowed for this endpoint."},
	{CodeServerNotFound, http.StatusNotFound, "No Pelican server container with this UUID."},
	{CodeServerNotRunning, http.StatusConflict, "The server container is not running."},
	{CodeNoJavaProcess, http.StatusConflict, "No Java process runs in the server container."},
	{CodeHsperfUnavailable, http.StatusConflict, "The JVM publishes no performance data (hsperfdata)."},
	{CodeAttachUnavailable, http.StatusConflict, "The bridge cannot attach to this JVM."},
	{CodeAttachFailed, http.StatusBadGateway, "The JVM rejected or failed the command."},
	{CodeAttachTimeout, http.StatusGatewayTimeout, "The JVM did not answer in time."},
	{CodeOutputTooLarge, http.StatusBadGateway, "The JVM output exceeds the size limit."},
	{CodeProfileRunning, http.StatusConflict, "A profile is already running for this server."},
	{CodeTooManyProfiles, http.StatusTooManyRequests, "Too many profiles are running on this node."},
	{CodeProfileNotFound, http.StatusNotFound, "No such profile (unknown, deleted or expired)."},
	{CodeInsufficientTmpSpace, http.StatusInsufficientStorage, "Not enough free space in the container's /tmp."},
	{CodeNotImplemented, http.StatusNotImplemented, "This endpoint is not implemented by this bridge version."},
	{CodeDockerUnavailable, http.StatusServiceUnavailable, "The Docker API is not reachable."},
	{CodeInternal, http.StatusInternalServerError, "Internal error."},
}

// apiError is an error response.
type apiError struct {
	status int
	body   ErrorBody
}

func (e *apiError) Error() string { return e.body.Error + ": " + e.body.Message }

// newError builds an error with the default message unless msg is given.
func newError(code string, msg string, details map[string]any) *apiError {
	for _, d := range Errors {
		if d.Code == code {
			if msg == "" {
				msg = d.Message
			}
			return &apiError{status: d.Status, body: ErrorBody{Error: code, Message: msg, Details: details}}
		}
	}
	return &apiError{status: http.StatusInternalServerError, body: ErrorBody{Error: CodeInternal, Message: msg, Details: details}}
}
