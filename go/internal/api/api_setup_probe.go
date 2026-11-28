package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
)

// FirstRunDriverTest serves POST /api/drivers/test before a box has any
// settings, so the setup guide can test each device as the owner adds it.
// It runs the same short probe Settings runs. With no saved settings there
// are no secrets to restore and no running driver to restart; nothing is
// written. deps needs only ConfigPath and the protocol factories.
func FirstRunDriverTest(deps Deps) http.HandlerFunc {
	s := &Server{deps: &deps}
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg config.Driver
		if err := readJSON(r, &cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid driver config: " + err.Error()})
			return
		}
		s.runDriverTest(w, r, cfg, nil)
	}
}
