package session

import "testing"

func TestCleanTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Production 500 errors", "Production 500 errors"},
		{"  \"Production 500 errors\"  ", "Production 500 errors"},
		{"Title: app.js failure.", "app.js failure"},
		{"**Credential refresh tokens**", "Credential refresh tokens"},
		{"\n\nfetch.ts retry logic\nignored second line", "fetch.ts retry logic"},
		{"", ""},
		// A model can answer the title request with chat-template control
		// syntax; it must never become a stored title.
		{"<｜｜DSML｜｜ calls>", ""},
		{"<||DSML|| calls>", ""},
		{"<|tool_calls|>", ""},
		{"<｜tool▁calls▁begin｜>", ""},
		{"DSML calls", ""},
		{"<|tool_calls|>\nReal title", "Real title"},
		{"Debug <|x|> tokens", "Debug tokens"},
	}
	for _, test := range cases {
		if got := CleanTitle(test.in); got != test.want {
			t.Fatalf("CleanTitle(%q) = %q want %q", test.in, got, test.want)
		}
	}
}

func TestCleanTitleBoundsLength(t *testing.T) {
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := CleanTitle(string(long))
	if len([]rune(got)) != maxTitleLength {
		t.Fatalf("want %d runes, got %d", maxTitleLength, len([]rune(got)))
	}
}

func TestUntitled(t *testing.T) {
	first := "fix the parser bug"
	if !Untitled("", first) {
		t.Fatal("empty title is untitled")
	}
	if !Untitled(titleFromText(first), first) {
		t.Fatal("automatic label is untitled")
	}
	if Untitled("Parser bug", first) {
		t.Fatal("a generated title is not untitled")
	}
}

func TestTitleInputFramesMessage(t *testing.T) {
	got := TitleInput("hello")
	if got == "" || !contains(got, "<message>") || !contains(got, "only the title") {
		t.Fatalf("unexpected framing: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
