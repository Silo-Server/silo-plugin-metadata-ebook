package metadata

import "testing"

func TestNormalizeISBN(t *testing.T) {
	cases := map[string]string{
		"978-0-593-13520-4": "9780593135204",
		" 0 306 40615 2 ":   "0306406152",
		"0-8044-2957-X":     "080442957X",
		"0-8044-2957-x":     "080442957X",
	}
	for input, want := range cases {
		if got := NormalizeISBN(input); got != want {
			t.Fatalf("NormalizeISBN(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeISBNRejectsShortValues(t *testing.T) {
	if got := NormalizeISBN("12345"); got != "" {
		t.Fatalf("NormalizeISBN short value = %q, want empty", got)
	}
}

func TestNormalizeISBNRejectsInvalidChecksums(t *testing.T) {
	cases := []string{
		"978-0-593-13520-5",
		"0-306-40615-3",
	}
	for _, input := range cases {
		if got := NormalizeISBN(input); got != "" {
			t.Fatalf("NormalizeISBN(%q) = %q, want empty", input, got)
		}
	}
}

func TestCanonicalISBNConvertsISBN10ToEquivalentISBN13(t *testing.T) {
	if got := CanonicalISBN("0-441-17271-7"); got != "9780441172719" {
		t.Fatalf("CanonicalISBN() = %q, want 9780441172719", got)
	}
}

func TestEquivalentISBNRecognizesISBN10AndISBN13Edition(t *testing.T) {
	if !EquivalentISBN("0-441-17271-7", "978-0-441-17271-9") {
		t.Fatal("EquivalentISBN() = false for equivalent ISBN-10 and ISBN-13")
	}
	if EquivalentISBN("0-441-17271-7", "978-0-593-13520-4") {
		t.Fatal("EquivalentISBN() = true for different editions")
	}
}
