package lyrics

import "testing"

func TestParseLRC(t *testing.T) {
	body := "[ar:Red Hot Chili Peppers]\n" +
		"[ti:Dani California]\n" +
		"[00:00.00]\n" +
		"[00:14.06]Getting born in the state of Mississippi\n" +
		"[00:18.50]Papa was a copper and her mama was a hippie\n" +
		"[01:14.000]California, rest in peace\n" +
		"[02:00]Late tag\n"

	got := ParseLRC(body)
	want := []Line{
		{TimeMS: 0, Text: ""},
		{TimeMS: 14060, Text: "Getting born in the state of Mississippi"},
		{TimeMS: 18500, Text: "Papa was a copper and her mama was a hippie"},
		{TimeMS: 74000, Text: "California, rest in peace"},
		{TimeMS: 120000, Text: "Late tag"},
	}

	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestParseLRC_MultipleStamps(t *testing.T) {
	// LRCLIB sometimes collapses repeated lyrics under multiple stamps.
	body := "[00:10.00][00:20.00]Chorus\n"
	got := ParseLRC(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(got))
	}
	if got[0].TimeMS != 10000 || got[1].TimeMS != 20000 {
		t.Errorf("timestamps wrong: %+v", got)
	}
	if got[0].Text != "Chorus" || got[1].Text != "Chorus" {
		t.Errorf("text wrong: %+v", got)
	}
}

func TestParseLRC_Empty(t *testing.T) {
	if got := ParseLRC(""); got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
}
