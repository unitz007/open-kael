package api

import (
	"encoding/json"
	"net/http"
)

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONList writes list as a JSON array, coercing a nil slice (what an
// empty Store.List* result returns) to [] rather than encoding/json's
// default null — a client shouldn't have to special-case "no results" as a
// different JSON type from "some results".
func writeJSONList[T any](w http.ResponseWriter, status int, list []T) {
	if list == nil {
		list = []T{}
	}
	writeJSON(w, status, list)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

// WriteError, WriteJSON, and DecodeJSON are the exported forms of their
// unexported counterparts, for use by provider plugins outside this package.
func WriteError(w http.ResponseWriter, status int, err error) { writeError(w, status, err) }
func WriteJSON(w http.ResponseWriter, status int, v any)      { writeJSON(w, status, v) }
func DecodeJSON(r *http.Request, v any) error                 { return decodeJSON(r, v) }
