// Package private provides primitives to interact with the openapi HTTP API.
//
// Code generated by github.com/algorand/oapi-codegen DO NOT EDIT.
package private

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	. "github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/oapi-codegen/pkg/runtime"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v4"
)

// ServerInterface represents all server handlers.
type ServerInterface interface {
	// Aborts a catchpoint catchup.
	// (DELETE /v2/catchup/{catchpoint})
	AbortCatchup(ctx echo.Context, catchpoint string) error
	// Starts a catchpoint catchup.
	// (POST /v2/catchup/{catchpoint})
	StartCatchup(ctx echo.Context, catchpoint string) error

	// (POST /v2/shutdown)
	ShutdownNode(ctx echo.Context, params ShutdownNodeParams) error
}

// ServerInterfaceWrapper converts echo contexts to parameters.
type ServerInterfaceWrapper struct {
	Handler ServerInterface
}

// AbortCatchup converts echo context to params.
func (w *ServerInterfaceWrapper) AbortCatchup(ctx echo.Context) error {
	var err error
	// ------------- Path parameter "catchpoint" -------------
	var catchpoint string

	err = runtime.BindStyledParameterWithLocation("simple", false, "catchpoint", runtime.ParamLocationPath, ctx.Param("catchpoint"), &catchpoint)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter catchpoint: %s", err))
	}

	ctx.Set(Api_keyScopes, []string{""})

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.AbortCatchup(ctx, catchpoint)
	return err
}

// StartCatchup converts echo context to params.
func (w *ServerInterfaceWrapper) StartCatchup(ctx echo.Context) error {
	var err error
	// ------------- Path parameter "catchpoint" -------------
	var catchpoint string

	err = runtime.BindStyledParameterWithLocation("simple", false, "catchpoint", runtime.ParamLocationPath, ctx.Param("catchpoint"), &catchpoint)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter catchpoint: %s", err))
	}

	ctx.Set(Api_keyScopes, []string{""})

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.StartCatchup(ctx, catchpoint)
	return err
}

// ShutdownNode converts echo context to params.
func (w *ServerInterfaceWrapper) ShutdownNode(ctx echo.Context) error {
	var err error

	ctx.Set(Api_keyScopes, []string{""})

	// Parameter object where we will unmarshal all parameters from the context
	var params ShutdownNodeParams
	// ------------- Optional query parameter "timeout" -------------

	err = runtime.BindQueryParameter("form", true, false, "timeout", ctx.QueryParams(), &params.Timeout)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter timeout: %s", err))
	}

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.ShutdownNode(ctx, params)
	return err
}

// This is a simple interface which specifies echo.Route addition functions which
// are present on both echo.Echo and echo.Group, since we want to allow using
// either of them for path registration
type EchoRouter interface {
	CONNECT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	HEAD(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	OPTIONS(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	TRACE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
}

// RegisterHandlers adds each server route to the EchoRouter.
func RegisterHandlers(router EchoRouter, si ServerInterface, m ...echo.MiddlewareFunc) {
	RegisterHandlersWithBaseURL(router, si, "", m...)
}

// Registers handlers, and prepends BaseURL to the paths, so that the paths
// can be served under a prefix.
func RegisterHandlersWithBaseURL(router EchoRouter, si ServerInterface, baseURL string, m ...echo.MiddlewareFunc) {

	wrapper := ServerInterfaceWrapper{
		Handler: si,
	}

	router.DELETE(baseURL+"/v2/catchup/:catchpoint", wrapper.AbortCatchup, m...)
	router.POST(baseURL+"/v2/catchup/:catchpoint", wrapper.StartCatchup, m...)
	router.POST(baseURL+"/v2/shutdown", wrapper.ShutdownNode, m...)

}

// Base64 encoded, gzipped, json marshaled Swagger object
var swaggerSpec = []string{

	"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",
	"1VOqN/X20+3/DwAA//+vckvpEccAAA==",
}

// GetSwagger returns the content of the embedded swagger specification file
// or error if failed to decode
func decodeSpec() ([]byte, error) {
	zipped, err := base64.StdEncoding.DecodeString(strings.Join(swaggerSpec, ""))
	if err != nil {
		return nil, fmt.Errorf("error base64 decoding spec: %s", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, fmt.Errorf("error decompressing spec: %s", err)
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(zr)
	if err != nil {
		return nil, fmt.Errorf("error decompressing spec: %s", err)
	}

	return buf.Bytes(), nil
}

var rawSpec = decodeSpecCached()

// a naive cached of a decoded swagger spec
func decodeSpecCached() func() ([]byte, error) {
	data, err := decodeSpec()
	return func() ([]byte, error) {
		return data, err
	}
}

// Constructs a synthetic filesystem for resolving external references when loading openapi specifications.
func PathToRawSpec(pathToFile string) map[string]func() ([]byte, error) {
	var res = make(map[string]func() ([]byte, error))
	if len(pathToFile) > 0 {
		res[pathToFile] = rawSpec
	}

	return res
}

// GetSwagger returns the Swagger specification corresponding to the generated code
// in this file. The external references of Swagger specification are resolved.
// The logic of resolving external references is tightly connected to "import-mapping" feature.
// Externally referenced files must be embedded in the corresponding golang packages.
// Urls can be supported but this task was out of the scope.
func GetSwagger() (swagger *openapi3.T, err error) {
	var resolvePath = PathToRawSpec("")

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(loader *openapi3.Loader, url *url.URL) ([]byte, error) {
		var pathToFile = url.String()
		pathToFile = path.Clean(pathToFile)
		getSpec, ok := resolvePath[pathToFile]
		if !ok {
			err1 := fmt.Errorf("path not found: %s", pathToFile)
			return nil, err1
		}
		return getSpec()
	}
	var specData []byte
	specData, err = rawSpec()
	if err != nil {
		return
	}
	swagger, err = loader.LoadFromData(specData)
	if err != nil {
		return
	}
	return
}
