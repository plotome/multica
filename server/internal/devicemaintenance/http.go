package devicemaintenance

import (
	"encoding/json"
	"io"
	"net/http"
)

// Serve is shared by the authenticated production handler and the isolated
// consumer fixture. Actor identity comes from authentication, never JSON.
func (s Store) Serve(w http.ResponseWriter, r *http.Request, device, actor, action string) {
	var body Request
	if r.Method == http.MethodPost {
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid maintenance binding", 400)
			return
		}
	} else if action != "status" && action != "capabilities" {
		http.Error(w, "POST required", 405)
		return
	}
	out, err := s.Act(r.Context(), device, actor, action, body)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code, b := PublicError(err)
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}
