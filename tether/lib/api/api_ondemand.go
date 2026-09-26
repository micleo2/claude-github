// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"net/http"
	"path"

	"github.com/syncthing/syncthing/lib/model"
)

func (s *service) onDemander(w http.ResponseWriter) (model.OnDemander, bool) {
	od, ok := s.model.(model.OnDemander)
	if !ok {
		http.Error(w, "on-demand files are not supported", http.StatusNotImplemented)
	}
	return od, ok
}

func (s *service) getOnDemandStatus(w http.ResponseWriter, r *http.Request) {
	od, ok := s.onDemander(w)
	if !ok {
		return
	}
	qs := r.URL.Query()
	files, err := od.OnDemandStatus(qs.Get("folder"), qs.Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if files == nil {
		files = []model.OnDemandFileState{}
	}
	sendJSON(w, files)
}

func (s *service) postOnDemand(w http.ResponseWriter, r *http.Request) {
	od, ok := s.onDemander(w)
	if !ok {
		return
	}
	qs := r.URL.Query()
	folder, p := qs.Get("folder"), qs.Get("path")
	var n int
	var err error
	switch path.Base(r.URL.Path) {
	case "pin":
		err = od.OnDemandPin(folder, p)
	case "unpin":
		err = od.OnDemandUnpin(folder, p)
	case "evict":
		n, err = od.OnDemandEvict(folder, p, qs.Get("verify") == "true")
	case "hydrate":
		n, err = od.OnDemandHydrate(r.Context(), folder, p)
	}
	res := map[string]any{"files": n}
	if err != nil {
		res["error"] = err.Error()
		w.WriteHeader(http.StatusConflict)
	}
	sendJSON(w, res)
}
