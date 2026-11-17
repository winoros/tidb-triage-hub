package model

import (
	"encoding/json"
	"testing"
)

func TestReviewVersionSurvivesPatchJSONRoundTrip(t *testing.T) {
	// Custom patch JSON preserves omission of force_keep/borderline while
	// carrying the same concurrency precondition across remote CLI hops.
	var patch TriagePatchRequest
	if err := json.Unmarshal([]byte(`{"cat":"HARD","severity":"MAJOR","subsystem":"logical_rewrite","expected_version":{"review":9,"notes":2}}`), &patch); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip TriagePatchRequest
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.ExpectedVersion == nil || *roundtrip.ExpectedVersion != (ReviewVersion{Review: 9, Notes: 2}) || roundtrip.HasForceKeepField() || roundtrip.HasBorderlineField() {
		t.Fatalf("patch changed across serialization: %s", raw)
	}
	var note NotePatchRequest
	if err := json.Unmarshal([]byte(`{"note":{"en":"Root cause"},"expected_version":{"review":9,"notes":2}}`), &note); err != nil {
		t.Fatal(err)
	}
	if note.ExpectedVersion == nil || *note.ExpectedVersion != *patch.ExpectedVersion || note.En != "Root cause" {
		t.Fatalf("note lost precondition: %+v", note)
	}
}
