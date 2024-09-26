// Copyright 2019 The ChubaoFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package objectnode

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

var (
	routeSNRegexp = regexp.MustCompile(":(\\w){32}$")
)

func IsMonitoredStatusCode(code int) bool {
	if code >= http.StatusInternalServerError {
		return true
	}
	return false
}

func generateWarnDetail(r *http.Request, errorInfo string) string {
	var (
		action     proto.Action
		bucket     string
		object     string
		requestID  string
		statusCode int
	)

	var param = ParseRequestParam(r)
	bucket = param.Bucket()
	object = param.Object()
	action = GetActionFromContext(r)
	requestID = GetRequestID(r)
	statusCode = GetStatusCodeFromContext(r)

	return fmt.Sprintf("Intenal Error!\n"+
		"Status: %v\n"+
		"RerquestID: %v\n"+
		"Action: %v\n"+
		"Bucket: %v\n"+
		"Object: %v\n"+
		"Error: %v",
		statusCode, requestID, action.Name(), bucket, object, errorInfo)
}

func (o *ObjectNode) crashMiddleware(next http.Handler) http.Handler {
	var handlerFunc http.HandlerFunc = func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				ServeInternalStaticErrorResponse(writer, request)
				exporter.Warning(generateWarnDetail(request, fmt.Sprintf("panic: %v", r)))
				log.LogCriticalf("Panic occurred: %v:\nCallstack:\n%v", r, string(debug.Stack()))
				return
			}
		}()
		next.ServeHTTP(writer, request)
	}
	return handlerFunc
}

type responseTracer struct{
	http.ResponseWriter
	statusCode int
}

func (t *responseTracer) Write(b []byte) (int, error) {
	if t.statusCode == 0 {
		t.statusCode = http.StatusOK
	}
	return t.ResponseWriter.Write(b)
}

func (t *responseTracer) WriteHeader(statusCode int) {
	t.ResponseWriter.WriteHeader(statusCode)
	if t.statusCode == 0 {
		t.statusCode = statusCode
	}
}

func (t *responseTracer) StatusCode() int {
	if t.statusCode == 0 {
		return http.StatusOK
	}
	return t.statusCode
}

func traceResponse(w http.ResponseWriter) *responseTracer {
	return &responseTracer{ResponseWriter: w, statusCode: 0}
}

func getStatusCodeFromResponseWriter(w http.ResponseWriter) int {
	if tracer, ok := w.(*responseTracer); ok {
		return tracer.StatusCode()
	}
	return http.StatusOK
}

// TraceMiddleware returns a middleware handler to trace request.
// After receiving the request, the handler will assign a unique RequestID to
// the request and record the processing time of the request.
// Workflow:
//   request → [pre-handle] → [next handler] → [post-handle] → response
func (o *ObjectNode) traceMiddleware(next http.Handler) http.Handler {
	var generateRequestID = func() (string, error) {
		var uUID uuid.UUID
		var err error
		if uUID, err = uuid.NewRandom(); err != nil {
			return "", err
		}
		return o.reqIDMask + strings.ReplaceAll(uUID.String(), "-", ""), nil
	}
	var handlerFunc http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		w = traceResponse(w)
		var err error

		// ===== pre-handle start =====
		var requestID string
		if requestID, err = generateRequestID(); err != nil {
			log.LogErrorf("traceMiddleware: generate request ID fail, remote(%v) url(%v) err(%v)",
				r.RemoteAddr, r.URL.String(), err)
			_ = InternalErrorCode(err).ServeResponse(w, r)
			// export ump warn info
			exporter.Warning(generateWarnDetail(r, err.Error()))
			return
		}

		// store request ID to context and write to header
		SetRequestID(r, requestID)
		w.Header()[HeaderNameXAmzRequestId] = []string{requestID}
		w.Header()[HeaderNameServer] = HeaderValueServerFullName

		var action = ActionFromRouteName(mux.CurrentRoute(r).GetName())
		SetRequestAction(r, action)

		// volume rate limit
		var param = ParseRequestParam(r)
		if param.Bucket() != "" {
			var vol *Volume
			if vol, err = o.getVol(param.Bucket()); err != nil && err == proto.ErrVolNotExists {
				_ = NoSuchBucket.ServeResponse(w, r)
				return
			}
			if err != nil {
				log.LogErrorf("traceMiddleware: load volume fail: requestID(%v) volume(%v) err(%v)",
					GetRequestID(r), param.Bucket(), err)
				_ = InternalErrorCode(err).ServeResponse(w, r)
				exporter.Warning(generateWarnDetail(r, err.Error()))
				return
			}
			err = vol.mw.CheckActionLimiter(context.Background(), action)
			if err != nil && err == syscall.EPERM {
				log.LogWarnf("traceMiddleware: volume action been limited, volume(%v), requestID(%v) action(%v) err(%v)",
					param.Bucket(), GetRequestID(r), action.String(), err)
				_ = TooManyRequests.ServeResponse(w, r)
				exporter.Warning(generateWarnDetail(r, fmt.Sprintf("volume action been limited, err(%s)", err.Error())))
				return
			}
			if err != nil {
				log.LogErrorf("traceMiddleware: volume action been limited, volume(%v), requestID(%v) action(%v) err(%v)",
					param.Bucket(), GetRequestID(r), action.String(), err)
				_ = InternalErrorCode(err).ServeResponse(w, r)
				exporter.Warning(generateWarnDetail(r, fmt.Sprintf("volume action been limited, err(%s)", err.Error())))
				return
			}
		}
		// ===== pre-handle finish =====

		var startTime = time.Now()
		metric := exporter.NewModuleTP(fmt.Sprintf("action_%v", action.Name()))
		defer func() {
			// failed request monitor
			var err error = nil
			if statusCode := getStatusCodeFromResponseWriter(w); IsMonitoredStatusCode(statusCode) {
				exporter.NewModuleTP(fmt.Sprintf("failed_%v", statusCode)).Set(nil)
				var errorMessage = getResponseErrorMessage(r)
				exporter.Warning(generateWarnDetail(r, errorMessage))
				err = errors.New(errorMessage)
			}
			metric.Set(err)
		}()

		// Check action is whether enabled.
		if !action.IsNone() && !o.disabledActions.Contains(action) {
			// next
			next.ServeHTTP(w, r)
		} else {
			// If current action is disabled, return access denied in response.
			if log.IsDebugEnabled() {
				log.LogDebugf("traceMiddleware: disabled action: requestID(%v) action(%v)", requestID, action.Name())
			}
			_ = AccessDenied.ServeResponse(w, r)
		}

		if statusCode := getStatusCodeFromResponseWriter(w); IsMonitoredStatusCode(statusCode) || log.IsDebugEnabled() {
			var headerToString = func(header http.Header) string {
				var sb = strings.Builder{}
				for k := range header {
					if sb.Len() != 0 {
						sb.WriteString(",")
					}
					sb.WriteString(fmt.Sprintf("%v:[%v]", k, header.Get(k)))
				}
				return "{" + sb.String() + "}"
			}

			if IsMonitoredStatusCode(statusCode) {
				log.LogErrorf("traceMiddleware: "+
					"action(%v) requestID(%v) host(%v) method(%v) url(%v) header(%v) "+
					"remote(%v) statusCode(%v) cost(%v)",
					action.Name(), requestID, r.Host, r.Method, r.URL.String(), headerToString(r.Header),
					getRequestIP(r), statusCode, time.Since(startTime))
			} else {
				log.LogDebugf("traceMiddleware: "+
					"action(%v) requestID(%v) host(%v) method(%v) url(%v) header(%v) "+
					"remote(%v) statusCode(%v) cost(%v)",
					action.Name(), requestID, r.Host, r.Method, r.URL.String(), headerToString(r.Header),
					getRequestIP(r), statusCode, time.Since(startTime))
			}

		}

	}
	return handlerFunc
}

// AuthMiddleware returns a pre-handle middleware handler to perform user authentication.
func (o *ObjectNode) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var currentAction = ActionFromRouteName(mux.CurrentRoute(r).GetName())
			if !currentAction.IsNone() && (proto.OSSOptionsObjectAction == currentAction || o.signatureIgnoredActions.Contains(currentAction)) {
				next.ServeHTTP(w, r)
				return
			}

			var (
				pass bool
				err  error
			)
			// check bucket public read
			if bucket := mux.Vars(r)["bucket"]; len(bucket) > 0 {
				var volume *Volume
				if volume, err = o.getVol(bucket); err != nil {
					if err == proto.ErrVolNotExists {
						_ = NoSuchBucket.ServeResponse(w, r)
						return
					}
					_ = InternalErrorCode(err).ServeResponse(w, r)
					return
				}
				if volume != nil && volume.isPublicRead() && currentAction.IsReadOnlyAction() {
					if log.IsDebugEnabled() {
						log.LogDebugf("authMiddleware: bucket is PublicRead: requestID(%v) volume(%v) action(%v)",
							GetRequestID(r), bucket, currentAction)
					}
					next.ServeHTTP(w, r)
					return
				}
			}
			//  check auth type
			if isHeaderUsingSignatureAlgorithmV4(r) {
				// using signature algorithm version 4 in header
				pass, err = o.validateHeaderBySignatureAlgorithmV4(r)
			} else if isHeaderUsingSignatureAlgorithmV2(r) {
				// using signature algorithm version 2 in header
				pass, err = o.validateHeaderBySignatureAlgorithmV2(r)
			} else if isUrlUsingSignatureAlgorithmV2(r) {
				// using signature algorithm version 2 in url parameter
				pass, err = o.validateUrlBySignatureAlgorithmV2(r)
			} else if isUrlUsingSignatureAlgorithmV4(r) {
				// using signature algorithm version 4 in url parameter
				pass, err = o.validateUrlBySignatureAlgorithmV4(r)
			}

			if err != nil {
				if err == proto.ErrVolNotExists {
					_ = NoSuchBucket.ServeResponse(w, r)
					return
				}
				_ = InternalErrorCode(err).ServeResponse(w, r)
				return
			}

			if !pass {
				_ = AccessDenied.ServeResponse(w, r)
				return
			}

			next.ServeHTTP(w, r)
		})
}

// PolicyCheckMiddleware returns a pre-handle middleware handler to process policy check.
// If action is configured in signatureIgnoreActions, then skip policy check.
func (o *ObjectNode) policyCheckMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			action := ActionFromRouteName(mux.CurrentRoute(r).GetName())
			if !action.IsNone() && o.signatureIgnoredActions.Contains(action) {
				next.ServeHTTP(w, r)
				return
			}
			wrappedNext := o.policyCheck(next.ServeHTTP)
			wrappedNext.ServeHTTP(w, r)
			return
		})
}

// ContentMiddleware returns a middleware handler to process reader for content.
// If the request contains the "X-amz-Decoded-Content-Length" header, it means that the data
// in the request body is chunked. Use ChunkedReader to parse the data.
// Workflow:
//   request → [pre-handle] → [next handler] → response
func (o *ObjectNode) contentMiddleware(next http.Handler) http.Handler {
	var handlerFunc http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header) > 0 && len(r.Header.Get(http.CanonicalHeaderKey(HeaderNameXAmzDecodeContentLength))) > 0 {
			r.Body = NewClosableChunkedReader(r.Body)
			if log.IsDebugEnabled() {
				log.LogDebugf("contentMiddleware: chunk reader inited: requestID(%v)", GetRequestID(r))
			}
		}
		next.ServeHTTP(w, r)
	}
	return handlerFunc
}

// Http's Expect header is a special header. When nginx is used as the reverse proxy in the front
// end of ObjectNode, nginx will process the Expect header information in advance, send the http
// status code 100 to the client, and will not forward this header information to ObjectNode.
// At this time, if the client request uses the Expect header when signing, it will cause the
// ObjectNode to verify the signature.
// A workaround is used here to solve this problem. Add the following configuration in nginx:
//   proxy_set_header X-Forwarded-Expect $ http_Expect
// In this way, nginx will not only automatically handle the Expect handshake, but also send
// the original value of Expect to the ObjectNode through X-Forwarded-Expect. ObjectNode only
// needs to use the value of X-Forwarded-Expect.
func (o *ObjectNode) expectMiddleware(next http.Handler) http.Handler {
	var handlerFunc http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		if forwardedExpect, originExpect := r.Header.Get(HeaderNameXForwardedExpect), r.Header.Get(HeaderNameExpect); forwardedExpect != "" && originExpect == "" {
			r.Header.Set(HeaderNameExpect, forwardedExpect)
		}
		next.ServeHTTP(w, r)
	}
	return handlerFunc
}

// CORSMiddleware returns a middleware handler to support CORS request.
// This handler will write following header into response:
//   Access-Control-Allow-Origin [*]
//   Access-Control-Allow-Headers [*]
//   Access-Control-Allow-Methods [*]
//   Access-Control-Max-Age [0]
// Workflow:
//   request → [pre-handle] → [next handler] → response
func (o *ObjectNode) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		var err error
		var param = ParseRequestParam(r)
		if param.Bucket() == "" {
			next.ServeHTTP(w, r)
			return
		}
		var vol *Volume
		if vol, err = o.vm.Volume(param.Bucket()); err != nil {
			next.ServeHTTP(w, r)
			return
		}

		var setupCORSHeader = func(volume *Volume, writer http.ResponseWriter, request *http.Request) {
			var origin = request.Header.Get(HeaderNameOrigin)
			var method = request.Header.Get(HeaderNameAccessControlRequestMethod)
			var headers []string
			if headerStr := request.Header.Get(HeaderNameAccessControlRequestHeaders); headerStr != "" {
				headers = strings.Split(headerStr, ",")
			}
			if origin == "" && method == "" && len(headers) == 0 {
				return
			}
			cors, _ := volume.metaLoader.loadCors()
			if cors != nil && len(cors.CORSRule) > 0 {
				for _, corsRule := range cors.CORSRule {
					if corsRule.Match(origin, method, headers) {
						// write access control allow headers
						if origin != "" {
							writer.Header()[HeaderNameAccessControlAllowOrigin] = []string{origin}
						}
						if method != "" {
							if len(corsRule.AllowedMethod) == 0 {
								writer.Header()[HeaderNameAccessControlAllowMethods] = HeaderValueAccessControlAllowMethodDefault
							} else {
								writer.Header()[HeaderNameAccessControlAllowMethods] = []string{strings.Join(corsRule.AllowedMethod, ",")}
							}
						}
						if len(headers) > 0 {
							if len(corsRule.AllowedHeader) == 0 {
								writer.Header()[HeaderNameAccessControlAllowHeaders] = HeaderValueAccessControlAllowHeadersDefault
							} else {
								writer.Header()[HeaderNameAccessControlAllowHeaders] = []string{strings.Join(corsRule.AllowedHeader, ",")}
							}
							if len(corsRule.ExposeHeader) == 0 {
								writer.Header()[HeaderNameAccessControlExposeHeaders] = HeaderValueAccessControlExposeHeadersDefault
							} else {
								writer.Header()[HeaderNameAccessControlExposeHeaders] = []string{strings.Join(corsRule.ExposeHeader, ",")}
							}
						}
						if corsRule.MaxAgeSeconds == 0 {
							writer.Header()[HeaderNameAccessControlMaxAge] = HeaderValueAccessControlMaxAgeDefault
						} else {
							writer.Header()[HeaderNameAccessControlMaxAge] = []string{strconv.Itoa(int(corsRule.MaxAgeSeconds))}
						}
						writer.Header()[HeaderNameAccessControlAllowCredentials] = HeaderValueAccessControlAllowCredentialsDefault
						return
					}
				}
			} else {
				// Write default CORS response headers
				if origin != "" {
					writer.Header()[HeaderNameAccessControlAllowOrigin] = []string{origin}
				}
				if method != "" {
					writer.Header()[HeaderNameAccessControlAllowMethods] = HeaderValueAccessControlAllowMethodDefault
				}
				if len(headers) > 0 {
					writer.Header()[HeaderNameAccessControlAllowHeaders] = HeaderValueAccessControlAllowHeadersDefault
					writer.Header()[HeaderNameAccessControlExposeHeaders] = HeaderValueAccessControlExposeHeadersDefault
				}
				writer.Header()[HeaderNameAccessControlMaxAge] = HeaderValueAccessControlMaxAgeDefault
				writer.Header()[HeaderNameAccessControlAllowCredentials] = HeaderValueAccessControlAllowCredentialsDefault
			}
		}
		setupCORSHeader(vol, w, r)
		next.ServeHTTP(w, r)
		return
	})
}
