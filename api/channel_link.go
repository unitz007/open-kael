package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

type redeemChannelCodeRequest struct {
	Code string `json:"code"`
}

// redeemChannelByCode handles POST /users/me/channels/redeem.
// Called by the web app when an authenticated user arrives via a
// "{frontendURL}/link?code=..." URL the bot generated. Looks up the code
// to find the (identity_id, channel_ref) the bot embedded, creates the
// MessengerChannel binding, and deletes the code (single use).
func (s *Server) redeemChannelByCode(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())

	var req redeemChannelCodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, errors.New("code is required"))
		return
	}

	lc, err := s.store.GetChannelLinkCode(r.Context(), req.Code)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid or expired code"))
		return
	}
	if lc.Expired() {
		s.store.DeleteChannelLinkCode(r.Context(), req.Code) //nolint:errcheck
		writeError(w, http.StatusUnauthorized, errors.New("invalid or expired code"))
		return
	}

	ch := &domain.MessengerChannel{
		ID:         newID(),
		UserID:     user.ID,
		IdentityID: lc.IdentityID,
		ChannelRef: lc.ChannelRef,
	}
	if err := s.store.SaveMessengerChannel(r.Context(), ch); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	s.store.DeleteChannelLinkCode(r.Context(), req.Code) //nolint:errcheck

	writeJSON(w, http.StatusOK, ch)
}
