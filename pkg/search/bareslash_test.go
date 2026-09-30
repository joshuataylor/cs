package search

import (
	"strings"
	"testing"
)

// lexAll returns every non-whitespace token of query, ending with EOF.
func lexAll(query string) []Token {
	lexer := NewLexer(strings.NewReader(query))
	var got []Token
	for {
		tok := lexer.scan()
		if tok.Type == WS {
			continue
		}
		got = append(got, tok)
		if tok.Type == EOF {
			return got
		}
	}
}

// A '/' opens a regex only at the START of a term. Inside a term it is an
// ordinary character, so a path or an import is one token.
//
// Before this, only a colon-filter value kept its slashes (path:pkg/search), so
// net/http lexed as KEYWORD(net) AND REGEX(/http/) — silently, because a space
// is a legal regex atom and the swallowed text only announced itself when it
// happened to be an invalid pattern.
func TestBareSlashInsideATermIsNotARegex(t *testing.T) {
	cases := []struct {
		query string
		want  []Token
	}{
		{"net/http", []Token{{Type: IDENTIFIER, Literal: "net/http"}, {Type: EOF}}},
		{"pkg/enterprise/api", []Token{{Type: IDENTIFIER, Literal: "pkg/enterprise/api"}, {Type: EOF}}},
		{"README.md and/or LICENSE", []Token{
			{Type: IDENTIFIER, Literal: "README.md"},
			{Type: IDENTIFIER, Literal: "and/or"},
			{Type: IDENTIFIER, Literal: "LICENSE"},
			{Type: EOF},
		}},
		{"path:pkg/search", []Token{{Type: IDENTIFIER, Literal: "path:pkg/search"}, {Type: EOF}}},
	}

	for _, c := range cases {
		got := lexAll(c.query)
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %v, want %v", c.query, got, c.want)
		}
		for i := range got {
			if got[i].Type != c.want[i].Type || got[i].Literal != c.want[i].Literal {
				t.Errorf("%q token %d: got %v, want %v", c.query, i, got[i], c.want[i])
			}
		}
	}
}

// The other half: a '/' at the start of a term is still a regex, wherever a
// term can start.
func TestBareSlashAtATermStartIsStillARegex(t *testing.T) {
	for _, query := range []string{"/foo/", "cat /foo/", "(cat dog)/foo/", `"phrase"/foo/`, "cat AND /foo/"} {
		found := false
		for _, tok := range lexAll(query) {
			if tok.Type == REGEX && tok.Literal == "foo" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no REGEX(foo) token in %v", query, lexAll(query))
		}
	}
}

// A regex whose closing '/' never comes swallows the rest of the query. That is
// now flagged on the token and reported by the parser, rather than searched
// for silently.
func TestAnUnterminatedRegexIsReported(t *testing.T) {
	var regex Token
	for _, tok := range lexAll("/foo bar") {
		if tok.Type == REGEX {
			regex = tok
		}
	}
	if regex.Literal != "foo bar" || !regex.Unterminated {
		t.Fatalf("got %+v, want an Unterminated REGEX(foo bar)", regex)
	}

	parser := NewParser(NewLexer(strings.NewReader("/foo bar")))
	_, notices := parser.ParseQuery()
	if !strings.Contains(strings.Join(notices, "\n"), "was not closed") {
		t.Errorf("no notice for an unterminated regex; notices: %v", notices)
	}

	// A closed regex carries no such flag and no such notice.
	parser = NewParser(NewLexer(strings.NewReader("/foo/ bar")))
	_, notices = parser.ParseQuery()
	if strings.Contains(strings.Join(notices, "\n"), "was not closed") {
		t.Errorf("a closed regex was reported as unclosed: %v", notices)
	}
}
