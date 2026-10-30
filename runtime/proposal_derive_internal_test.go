package runtime

import (
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestYesNoToAnOpenQuestion(t *testing.T) {
	yesNo := []domain.ProposalChoice{{Label: "Yes, draft it", Say: "Yes"}, {Label: "No, wait", Say: "No"}}
	two := []domain.ProposalChoice{{Label: "Tomorrow morning", Say: "a"}, {Label: "This evening at 7", Say: "b"}}
	for reply, want := range map[string]bool{
		"Sure. What's your landlord's email address?":                true,
		"Sure. When should I remind you: tomorrow morning, or at 7?": false, // ends "or at 7?": not a wh-question as cut
		"The 10:15 slot on Tuesday is free. Should I book it?":       false,
		"I found 12 newsletters. Archive all 12?":                    false,
		"Paris.": false,
	} {
		if got := yesNoToAnOpenQuestion(reply, yesNo); got != want {
			t.Errorf("%q: %v", reply, got)
		}
	}
	if yesNoToAnOpenQuestion("When should I remind you?", two) {
		t.Error("choices that aren't yes and no stay")
	}
}
