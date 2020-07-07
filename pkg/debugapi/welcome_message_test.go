package debugapi_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/ethersphere/bee/pkg/debugapi"
	"github.com/ethersphere/bee/pkg/jsonhttp"
	"github.com/ethersphere/bee/pkg/jsonhttp/jsonhttptest"
	"github.com/ethersphere/bee/pkg/p2p/mock"
)

var (
	testWelcomeMessage string
)

func TestGetWelcomeMessage(t *testing.T) {
	const DefaultTestWelcomeMessage = "Hello World!"

	srv := newTestServer(t, testServerOptions{
		P2P: mock.New(mock.WithWelcomeMessageHandlerFunc(nil, func() string {
			return DefaultTestWelcomeMessage
		})),
	})

	jsonhttptest.ResponseDirect(t, srv.Client, http.MethodGet, "/welcome-message", nil, http.StatusOK, debugapi.WelcomeMessageResponse{
		WelcomeMesssage: DefaultTestWelcomeMessage,
	})
}

func TestSetWelcomeMessage(t *testing.T) {
	const NewWelcomeMessage = "Changed value"

	testWelcomeMessage := ""
	srv := newTestServer(t, testServerOptions{
		P2P: mock.New(mock.WithWelcomeMessageHandlerFunc(func(val string) error {
			testWelcomeMessage = val
			return nil
		}, nil)),
	})

	jsonhttptest.ResponseDirect(t, srv.Client, http.MethodPost, "/welcome-message", bytes.NewReader([]byte(NewWelcomeMessage)), http.StatusOK, jsonhttp.StatusResponse{
		Message: "OK",
		Code:    http.StatusOK,
	})

	if testWelcomeMessage != NewWelcomeMessage {
		t.Fatalf("Bad welcome message: want %s, got %s", NewWelcomeMessage, testWelcomeMessage)
	}
}
