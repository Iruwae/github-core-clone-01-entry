package main

import "testing"

func TestViewerDerivation(t *testing.T) {
	if viewer.login != "adaiversen" || viewer.id != 1000 {
		t.Fatalf("viewer = %s/%d, want adaiversen/1000", viewer.login, viewer.id)
	}
	if teammate.login != "quinnkeller" || teammate.id != 1001 {
		t.Fatalf("teammate = %s/%d, want quinnkeller/1001", teammate.login, teammate.id)
	}
	if stranger.login != "olgabauer" || stranger.id != 1002 {
		t.Fatalf("stranger = %s/%d, want olgabauer/1002", stranger.login, stranger.id)
	}
}
