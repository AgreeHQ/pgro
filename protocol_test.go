package main

import "testing"

func TestLeadingKeyword(t *testing.T) {
	cases := map[string]string{
		"select 1":                                "select",
		"  \n\tSELECT 1":                          "select",
		"(select 1) union (select 2)":             "select",
		"-- hi\nwith x as (select 1) table x":     "with",
		"/* a */ explain select 1":                "explain",
		"/* /* nested */ select */ delete from t": "delete",
		"/* unterminated select":                  "",
		"-- only a comment":                       "",
		"insert into t values (1)":                "insert",
		"Show work_mem":                           "show",
		"":                                        "",
	}
	for in, want := range cases {
		if got := leadingKeyword(in); got != want {
			t.Errorf("leadingKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}
