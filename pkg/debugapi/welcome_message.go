// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package debugapi

import (
	"io/ioutil"
	"net/http"

	"github.com/ethersphere/bee/pkg/jsonhttp"
)

type welcomeMessageResponse struct {
	WelcomeMesssage string `json:"welcome_message"`
}

func (s *server) welcomeMessageSyncedHandler(w http.ResponseWriter, r *http.Request) {
	val := s.P2P.WelcomeMessageSynced()
	jsonhttp.OK(w, welcomeMessageResponse{
		WelcomeMesssage: val,
	})
}

func (s *server) setWelcomeMessageHandler(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.Logger.Debugf("welcome message: read error: %v", err)
		s.Logger.Error("welcome message: read error")
		jsonhttp.InternalServerError(w, "cannot read welcome message")
		return
	}
	s.P2P.SetWelcomeMessage(string(data))
	jsonhttp.OK(w, nil)
}
