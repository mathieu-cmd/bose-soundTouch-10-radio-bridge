package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type stationData struct {
	Name      string `json:"name"`
	StreamURL string `json:"streamUrl"`
	ImageURL  string `json:"imageUrl,omitempty"`
}

const defaultStationName = "Internet Radio"

type logLevel int

const (
	logLevelDebug logLevel = iota
	logLevelInfo
)

var currentLogLevel = parseLogLevel(strings.TrimSpace(os.Getenv("LOG_LEVEL")))

func parseLogLevel(v string) logLevel {
	switch strings.ToLower(v) {
	case "debug":
		return logLevelDebug
	default:
		return logLevelInfo
	}
}

func logDebugf(format string, args ...any) {
	if currentLogLevel <= logLevelDebug {
		log.Printf("DEBUG "+format, args...)
	}
}

func logInfof(format string, args ...any) {
	if currentLogLevel <= logLevelInfo {
		log.Printf("INFO "+format, args...)
	}
}

type bmxRegistryResponse struct {
	Services map[string]string `json:"services"`
}

type playbackAudioStream struct {
	HasPlaylist       bool   `json:"hasPlaylist"`
	IsRealtime        bool   `json:"isRealtime"`
	BufferingTimeout  int    `json:"bufferingTimeout,omitempty"`
	ConnectingTimeout int    `json:"connectingTimeout,omitempty"`
	MaxTimeout        int    `json:"maxTimeout,omitempty"`
	StreamURL         string `json:"streamUrl"`
}

type playbackAudio struct {
	HasPlaylist bool                  `json:"hasPlaylist"`
	IsRealtime  bool                  `json:"isRealtime"`
	MaxTimeout  int                   `json:"maxTimeout,omitempty"`
	StreamURL   string                `json:"streamUrl"`
	Streams     []playbackAudioStream `json:"streams"`
}

type orionStationResponse struct {
	Audio      playbackAudio `json:"audio"`
	ImageURL   string        `json:"imageUrl"`
	IsFavorite bool          `json:"isFavorite"`
	Name       string        `json:"name"`
	StreamType string        `json:"streamType"`
}

func main() {
	if runningInLambda() {
		runLambda()
		return
	}

	runHTTPServer()
}

func runHTTPServer() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	addr := "0.0.0.0:" + port
	logInfof("bose-cloud-bridge listening on %s", addr)
	h := requestLogger(buildMux())
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func runLambda() {
	logInfof("bose-cloud-bridge running in lambda mode")
	h := requestLogger(buildMux())
	adapter := &lambdaAdapter{handler: h}
	lambda.Start(adapter.Handle)
}

func runningInLambda() bool {
	return strings.TrimSpace(os.Getenv("AWS_LAMBDA_RUNTIME_API")) != ""
}

type lambdaAdapter struct {
	handler http.Handler
}

func (a *lambdaAdapter) Handle(ctx context.Context, event events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	req, err := requestFromLambdaEvent(ctx, event)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Headers: map[string]string{
				"Content-Type": "text/plain; charset=utf-8",
			},
			Body: "invalid request: " + err.Error(),
		}, nil
	}

	rr := httptest.NewRecorder()
	a.handler.ServeHTTP(rr, req)

	return lambdaResponseFromRecorder(rr), nil
}

func requestFromLambdaEvent(ctx context.Context, event events.APIGatewayV2HTTPRequest) (*http.Request, error) {
	method := strings.TrimSpace(event.RequestContext.HTTP.Method)
	if method == "" {
		method = http.MethodGet
	}

	path := strings.TrimSpace(event.RawPath)
	if path == "" {
		path = strings.TrimSpace(event.RequestContext.HTTP.Path)
	}
	if path == "" {
		path = "/"
	}
	path = collapseSlashes(path)

	bodyBytes := []byte(event.Body)
	if event.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return nil, fmt.Errorf("decode body: %w", err)
		}
		bodyBytes = decoded
	}

	req, err := http.NewRequestWithContext(ctx, method, "/", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Build the URL directly instead of letting it be re-parsed from a string:
	// a path starting with "//" is otherwise read by url.Parse as a network-path
	// reference, moving the leading segment into URL.Host and emptying URL.Path,
	// which then makes ServeMux redirect (307) to "/".
	req.URL = &url.URL{
		Path:     path,
		RawQuery: event.RawQueryString,
	}

	for k, v := range event.Headers {
		req.Header.Set(k, v)
	}

	host := strings.TrimSpace(event.Headers["host"])
	if host == "" {
		host = strings.TrimSpace(event.RequestContext.DomainName)
	}
	if host != "" {
		req.Host = host
	}

	if req.Header.Get("X-Forwarded-Proto") == "" {
		if proto := strings.TrimSpace(event.Headers["x-forwarded-proto"]); proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		} else {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
	}

	if ip := strings.TrimSpace(event.RequestContext.HTTP.SourceIP); ip != "" {
		req.RemoteAddr = ip
	}

	return req, nil
}

// collapseSlashes replaces runs of consecutive slashes with a single slash so
// ServeMux's clean-path redirect never triggers for paths like "//healthz".
func collapseSlashes(path string) string {
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	return path
}

func lambdaResponseFromRecorder(rr *httptest.ResponseRecorder) events.APIGatewayV2HTTPResponse {
	res := rr.Result()
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	headers := map[string]string{}
	cookies := []string{}

	for key, values := range res.Header {
		if strings.EqualFold(key, "Set-Cookie") {
			cookies = append(cookies, values...)
			continue
		}
		headers[key] = strings.Join(values, ",")
	}

	statusCode := res.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode:      statusCode,
		Headers:         headers,
		Cookies:         cookies,
		Body:            string(body),
		IsBase64Encoded: false,
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logDebugf("request method=%s path=%s host=%s ua=%q", r.Method, r.URL.Path, r.Host, r.UserAgent())
		next.ServeHTTP(w, r)
	})
}

func buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	registerBridgeRoutes(mux, "", "")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logDebugf("root path=%s", r.URL.Path)
		if r.URL.Path != "/" {
			logDebugf("unmatched method=%s path=%s host=%s ua=%q", r.Method, r.URL.Path, r.Host, r.UserAgent())
			respondCatchAll(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bmxRegistry(w, r, "")
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

func registerAccountHandlers(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/account/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/device") && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			accountDevice(w, r)
			return
		}
		if !(strings.HasSuffix(r.URL.Path, "/full") || strings.HasSuffix(r.URL.Path, "/provider_settings") || strings.HasSuffix(r.URL.Path, "/sources")) {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sources") {
			accountSources(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/provider_settings") {
			accountProviderSettings(w, r)
			return
		}
		accountFull(w, r)
	})
}

func parseAccountID(path, suffix string) string {
	trimmed := strings.Trim(strings.TrimSuffix(path, suffix), "/")
	parts := strings.Split(trimmed, "/")
	for i := 0; i < len(parts); i++ {
		if parts[i] == "account" {
			if i+1 >= len(parts) || parts[i+1] == "" {
				return "unknown"
			}
			return parts[i+1]
		}
	}

	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return "unknown"
	}
	return parts[len(parts)-1]
}

func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func extractEncodedStationData(r *http.Request) string {
	encoded := strings.TrimSpace(r.URL.Query().Get("data"))
	if encoded != "" {
		return encoded
	}

	encoded = strings.TrimPrefix(r.URL.Path, "/core02/svc-bmx-adapter-orion/prod/orion/")
	encoded = strings.TrimPrefix(encoded, "/svc-bmx-adapter-orion/prod/orion/")
	encoded = strings.TrimPrefix(encoded, "/orion/")
	encoded = strings.TrimPrefix(encoded, "station/")
	return strings.Trim(encoded, "/")
}

func respondCatchAll(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
		writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func registerBridgeRoutes(mux *http.ServeMux, routePrefix string, orionBasePath string) {
	mux.HandleFunc(routePrefix+"/marge/streaming/support/power_on", func(w http.ResponseWriter, r *http.Request) {
		powerOn(w, r)
	})
	mux.HandleFunc(routePrefix+"/streaming/support/power_on", func(w http.ResponseWriter, r *http.Request) {
		powerOn(w, r)
	})

	registerAccountHandlers(mux, routePrefix+"/marge/streaming")
	mux.HandleFunc(routePrefix+"/marge/streaming/sourceproviders", sourceProviders)
	mux.HandleFunc(routePrefix+"/marge/streaming/device/", streamingToken)
	registerAccountHandlers(mux, routePrefix+"/streaming")
	mux.HandleFunc(routePrefix+"/streaming/sourceproviders", sourceProviders)
	mux.HandleFunc(routePrefix+"/streaming/device/", streamingToken)

	mux.HandleFunc(routePrefix+"/bmx/registry/v1/services", func(w http.ResponseWriter, r *http.Request) {
		bmxRegistry(w, r, orionBasePath)
	})
	mux.HandleFunc(routePrefix+"/registry/v1/services", func(w http.ResponseWriter, r *http.Request) {
		bmxRegistry(w, r, orionBasePath)
	})

	mux.HandleFunc(routePrefix+"/orion/station", orionStation)
	mux.HandleFunc(routePrefix+"/orion/", orionStation)
	mux.HandleFunc(routePrefix+"/svc-bmx-adapter-orion/prod/orion/station", orionStation)
	mux.HandleFunc(routePrefix+"/svc-bmx-adapter-orion/prod/orion/", orionStation)
	mux.HandleFunc(routePrefix+"/core02/svc-bmx-adapter-orion/prod/orion/station", orionStation)
	mux.HandleFunc(routePrefix+"/core02/svc-bmx-adapter-orion/prod/orion/", orionStation)
}

func powerOn(w http.ResponseWriter, r *http.Request) {
	logInfof("power_on path=%s", r.URL.Path)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

func accountFull(w http.ResponseWriter, r *http.Request) {
	logDebugf("account_full path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accountID := parseAccountID(r.URL.Path, "/full")

	writeBoseXML(w, http.StatusOK, accountFullXML(accountID))
}

// accountDevice answers the speaker's device registration (POST) or rename (PUT)
// on /streaming/account/{account}/device[/{device}]. The speaker sends this
// during /setMargeAccount pairing and refuses to pair without a valid answer.
func accountDevice(w http.ResponseWriter, r *http.Request) {
	logDebugf("account_device method=%s path=%s", r.Method, r.URL.Path)
	accountID := parseAccountID(r.URL.Path, "")

	var dev struct {
		DeviceID   string `xml:"deviceid,attr"`
		Name       string `xml:"name"`
		MACAddress string `xml:"macaddress"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = xml.Unmarshal(body, &dev)
	if dev.DeviceID == "" {
		// PUT .../device/{deviceID}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "device" {
			dev.DeviceID = parts[len(parts)-1]
		}
	}
	if dev.MACAddress == "" {
		dev.MACAddress = dev.DeviceID
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000+00:00")
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(dev.Name))
	name := b.String()
	res := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><device deviceid="%s"><createdOn>%s</createdOn><ipaddress></ipaddress><name>%s</name><updatedOn>%s</updatedOn></device>`, dev.DeviceID, now, name, now)

	w.Header().Set("Content-Type", "application/vnd.bose.streaming-v1.2+xml")
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
		w.Header().Set("Location", requestScheme(r)+"://"+r.Host+"/streaming/account/"+accountID+"/device/"+dev.DeviceID)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(res))
}

func accountProviderSettings(w http.ResponseWriter, r *http.Request) {
	logDebugf("account_provider_settings path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accountID := parseAccountID(r.URL.Path, "/provider_settings")

	writeBoseXML(w, http.StatusOK, providerSettingsXML(accountID))
}

func sourceProviders(w http.ResponseWriter, r *http.Request) {
	logDebugf("source_providers path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeBoseXML(w, http.StatusOK, sourceProvidersXML())
}

func accountSources(w http.ResponseWriter, r *http.Request) {
	logDebugf("account_sources path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeBoseXML(w, http.StatusOK, accountSourcesXML())
}

func streamingToken(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/streaming_token") {
		http.NotFound(w, r)
		return
	}
	logDebugf("streaming_token path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Authorization", "c3dvcmRmaXNoCg==")
	w.Header().Set("ETag", fmt.Sprintf("%d", time.Now().UnixMilli()))
	w.WriteHeader(http.StatusOK)
}

func bmxRegistry(w http.ResponseWriter, r *http.Request, basePath string) {
	logDebugf("bmx_registry path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	scheme := requestScheme(r)
	serviceOrigin := scheme + "://" + r.Host
	orionBaseURL := serviceOrigin + basePath + "/core02/svc-bmx-adapter-orion/prod/orion"
	orionStationURL := serviceOrigin + basePath + "/orion/station"

	writeJSON(w, http.StatusOK, map[string]any{
		"_links": map[string]any{
			"bmx_services_availability": map[string]string{"href": "../servicesAvailability"},
		},
		"askAgainAfter": 1230482,
		"bmx_services": []map[string]any{
			{
				"_links": map[string]any{
					"bmx_token": map[string]string{"href": "/token"},
					"self":      map[string]string{"href": "/"},
				},
				"askAdapter": false,
				"assets": map[string]any{
					"color":       "#000000",
					"description": "Custom radio stations with BMX.",
					"name":        "Custom Stations",
				},
				"authenticationModel": map[string]any{
					"anonymousAccount": map[string]any{
						"autoCreate": true,
						"enabled":    true,
					},
				},
				"baseUrl": orionBaseURL,
				"id": map[string]any{
					"name":  "LOCAL_INTERNET_RADIO",
					"value": 11,
				},
				"streamTypes": []string{"liveRadio"},
			},
		},
		// Keep legacy compatibility with earlier simple bridge clients.
		"services": map[string]string{
			"orion": orionStationURL,
		},
	})
}

func orionStation(w http.ResponseWriter, r *http.Request) {
	logDebugf("orion_station path=%s", r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	encoded := extractEncodedStationData(r)
	if encoded == "" {
		http.Error(w, "missing data parameter", http.StatusBadRequest)
		return
	}

	station, err := decodeStationData(encoded)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid data: %v", err), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, orionStationResponse{
		Audio: playbackAudio{
			HasPlaylist: true,
			IsRealtime:  true,
			MaxTimeout:  60,
			StreamURL:   station.StreamURL,
			Streams: []playbackAudioStream{
				{
					HasPlaylist:       true,
					IsRealtime:        true,
					BufferingTimeout:  20,
					ConnectingTimeout: 10,
					MaxTimeout:        60,
					StreamURL:         station.StreamURL,
				},
			},
		},
		ImageURL:   station.ImageURL,
		IsFavorite: false,
		Name:       station.Name,
		StreamType: "liveRadio",
	})
}

func decodeStationData(encoded string) (stationData, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return stationData{}, err
	}

	var station stationData
	if err := json.Unmarshal(decoded, &station); err != nil {
		return stationData{}, err
	}
	if station.StreamURL == "" {
		return stationData{}, fmt.Errorf("streamUrl is required")
	}
	if station.Name == "" {
		station.Name = defaultStationName
	}

	return station, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeBoseXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/vnd.bose.streaming-v1.2+xml")
	w.Header().Set("ETag", fmt.Sprintf("%d", time.Now().UnixMilli()))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func sourceProvidersXML() string {
	now := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><sourceProviders><sourceprovider id="11"><createdOn>%s</createdOn><name>LOCAL_INTERNET_RADIO</name><updatedOn>%s</updatedOn></sourceprovider></sourceProviders>`, now, now)
}

func providerSettingsXML(accountID string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><providerSettings><providerSetting><boseId>%s</boseId><keyName>ELIGIBLE_FOR_TRIAL</keyName><value>true</value><providerId>14</providerId></providerSetting></providerSettings>`, accountID)
}

func accountFullXML(accountID string) string {
	now := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><account id="%s"><accountStatus>OK</accountStatus><devices></devices><mode>global</mode><preferredLanguage>en</preferredLanguage><providerSettings><providerSetting><boseId>%s</boseId><keyName>ELIGIBLE_FOR_TRIAL</keyName><value>true</value><providerId>14</providerId></providerSetting></providerSettings><sources><source id="11" type="Audio"><createdOn>%s</createdOn><credential type="token"></credential><name></name><sourceproviderid>11</sourceproviderid><sourcename>Custom Stations</sourcename><sourceSettings></sourceSettings><updatedOn>%s</updatedOn><username></username></source></sources></account>`, accountID, accountID, now, now)
}

func accountSourcesXML() string {
	now := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><sources><source id="11" type="Audio"><createdOn>%s</createdOn><credential type="token"></credential><name></name><sourceproviderid>11</sourceproviderid><sourcename>Custom Stations</sourcename><sourceSettings></sourceSettings><updatedOn>%s</updatedOn><username></username></source></sources>`, now, now)
}
