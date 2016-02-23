package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/jonasfj/statsum/aggregator"
	"github.com/jonasfj/statsum/payload"
)

// Config is the options for StatSum server
type Config struct {
	Port          string
	ForceSsl      bool
	JwtSecret     []byte
	SignalFxToken string
	DatadogAPIKey string
	DatadogAppKey string
}

// StatSum is a server instance.
type StatSum struct {
	config     Config
	server     http.Server
	aggregator *aggregator.Aggregator
}

// New returns a new StatSum
func New(config Config) (*StatSum, error) {
	s := StatSum{
		config:     config,
		server:     http.Server{},
		aggregator: aggregator.NewAggregator(),
	}
	s.server.Addr = fmt.Sprintf(":%s", config.Port)
	s.server.Handler = http.HandlerFunc(s.handler)
	s.server.ReadTimeout = 5000 * time.Second
	s.server.WriteTimeout = 25 * time.Second
	s.server.MaxHeaderBytes = 1 << 20
	return &s, nil
}

// Start will start the server
func (s *StatSum) Start() error {
	go func() {
		for {
			time.Sleep(5 * time.Second)
			s.printHealthMetrics()
		}
	}()
	go s.scheduleSubmission()
	return s.server.ListenAndServe()
}

func (s *StatSum) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/data/") && len(r.URL.Path) > 6 {
		project := r.URL.Path[6:]
		// Validate project
		if !validateProjectName(project) {
			reply(w, http.StatusBadRequest, payload.Response{
				Code:    "InvalidProject",
				Message: "Project name can only use characters [0-9a-zA-Z_-]",
			}, detectResponseType(r, NoFormat))
			return
		}
		if !s.authorize(project, w, r) {
			return
		}
		s.parse(project, w, r)
		return
	}

	reply(w, http.StatusNotFound, payload.Response{
		Code:    "ResourceNotFound",
		Message: "No such API end-point",
	}, detectResponseType(r, NoFormat))
}

var errInvalidAlgorithm = errors.New("Invalid signature algorithm")

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}
type jwtClaims struct {
	Project string `json:"project"`
}

func (s *StatSum) authorize(project string, w http.ResponseWriter, r *http.Request) bool {
	authorization := r.Header.Get("Authorization")

	if len(authorization) > 7 && strings.EqualFold(authorization[0:7], "bearer ") {
		token := authorization[7:]
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			goto failed
		}
		rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			goto failed
		}
		header := jwtHeader{}
		err = json.Unmarshal(rawHeader, &header)
		if err != nil {
			goto failed
		}
		if header.Algorithm != "HS256" || header.Type != "JWT" {
			goto failed
		}

		signingString := token[:len(parts[0])+len(parts[1])+1]
		rawSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
		mac := hmac.New(sha256.New, s.config.JwtSecret)
		mac.Write([]byte(signingString))
		if !hmac.Equal(mac.Sum(nil), rawSignature) {
			goto failed
		}

		rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			goto failed
		}
		claims := jwtClaims{}
		err = json.Unmarshal(rawClaims, &claims)
		if err != nil {
			goto failed
		}

		if claims.Project == project {
			return true
		}
		reply(w, http.StatusForbidden, payload.Response{
			Code:    "AuthorizationFailed",
			Message: "The given JWT does not grant access to this project!",
		}, detectResponseType(r, NoFormat))
		return false
	}

failed:
	w.Header().Set("Www-Authenticate", "Bearer")
	reply(w, http.StatusUnauthorized, payload.Response{
		Code:    "AuthenticationFailed",
		Message: "The request must carry a valid JWT",
	}, detectResponseType(r, NoFormat))
	return false
}

func (s *StatSum) parse(project string, w http.ResponseWriter, r *http.Request) {
	// Read body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		reply(w, http.StatusBadRequest, payload.Response{
			Code:    "InvalidPayload",
			Message: "Failed to read the entire payload",
		}, detectResponseType(r, NoFormat))
		return
	}

	// Parse body
	contentType := detectContentType(r.Header.Get("Content-Type"))
	p := payload.Payload{}
	switch contentType {
	case JSONFormat:
		// Parse JSON
		err = p.UnmarshalJSON(body)
		if err != nil {
			reply(w, http.StatusBadRequest, payload.Response{
				Code:    "InvalidPayload",
				Message: "Failed to parse JSON payload, error: " + err.Error(),
			}, detectResponseType(r, contentType))
			return
		}
	case MsgPackFormat:
		_, err := p.UnmarshalMsg(body)
		if err != nil {
			reply(w, http.StatusBadRequest, payload.Response{
				Code:    "InvalidPayload",
				Message: "Failed to parse msgpack payload, error: " + err.Error(),
			}, detectResponseType(r, contentType))
			return
		}
	default:
		reply(w, http.StatusUnsupportedMediaType, payload.Response{
			Code:    "InvalidContentType",
			Message: "Accepted content-types: application/json, application/msgpack",
		}, detectResponseType(r, contentType))
		return
	}

	s.process(project, &p)

	// Send a response 200 OK reply
	reply(w, http.StatusOK, payload.Response{
		Code:    "PayloadAccepted",
		Message: "Payload have been aggregated",
	}, detectResponseType(r, contentType))
}

func (s *StatSum) process(project string, payload *payload.Payload) {
	s.aggregator.Process(project, payload)
}

func (s *StatSum) printHealthMetrics() {
	mem := runtime.MemStats{}
	runtime.ReadMemStats(&mem)
	fmt.Println("-----------")
	fmt.Println("Memory usage: ", mem.Alloc/(1024*1024), " mb")
	s.aggregator.PrintHealthMetrics()
}

func (s *StatSum) scheduleSubmission() {
	for {
		now := time.Now().UTC()
		nextTime := now.Truncate(5 * time.Minute).Add(5 * time.Minute)
		time.Sleep(nextTime.Sub(now) + 5*time.Second)

		submitter := func(name string, value float64) {
			// nextTime
		}
		s.aggregator.SendMetrics(submitter)
		if nextTime.Equal(now.Truncate(time.Hour).Add(time.Hour)) {
			s.aggregator.SendHourlyMetrics(submitter)
		}
	}
}