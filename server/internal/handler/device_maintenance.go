package handler

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/devicemaintenance"
	"net/http"
)

func (h *Handler) DeviceMaintenance(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "human maintenance only")
		return
	}
	(devicemaintenance.Store{DB: h.TxStarter}).Serve(w, r, chi.URLParam(r, "daemonId"), requestUserID(r), chi.URLParam(r, "action"))
}
