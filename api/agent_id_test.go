package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/unitz007/open-kael/api"
)

// A screen may choose a new agent's id in advance, so it can show the agent as it will look. The id is used only when
// it has the usual form and no agent has it: it can never reach, or overwrite, an existing agent.
func TestCreateAgent_AProposedIdIsUsedOnlyWhenItIsFresh(t *testing.T) {
	store := openDB(t)
	srv := api.NewServer(store)
	create := func(id, name string) (string, int) {
		rec := postJSON(t, srv, "/agents", map[string]any{"id": id, "name": name, "max_iterations": 5})
		var got struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got.ID, rec.Code
	}
	const fresh = "0123456789abcdef0123456789abcdef"
	t.Cleanup(func() { _ = store.DeleteAgent(t.Context(), fresh) })
	if id, code := create(fresh, "First"); code != http.StatusOK || id != fresh {
		t.Fatalf("a fresh id is used: %d %q", code, id)
	}
	again, code := create(fresh, "Intruder")
	if code != http.StatusOK || again == fresh || again == "" {
		t.Fatalf("an id an agent already has gets a new one instead: %d %q", code, again)
	}
	t.Cleanup(func() { _ = store.DeleteAgent(t.Context(), again) })
	if a, err := store.GetAgent(t.Context(), fresh); err != nil || a.Name != "First" {
		t.Errorf("the existing agent is untouched: %+v %v", a, err)
	}
	for _, bad := range []string{"short", "../../etc", "0123456789ABCDEF0123456789ABCDEF", ""} {
		id, _ := create(bad, "Odd")
		if id == bad || len(id) != 32 {
			t.Errorf("an id of another form is not used: %q → %q", bad, id)
		}
		t.Cleanup(func() { _ = store.DeleteAgent(t.Context(), id) })
	}
}
