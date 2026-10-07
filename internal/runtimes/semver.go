package runtimes

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a release version, major.minor.patch. Pre-release and build
// suffixes are ignored: toolchains are picked from final releases only.
type Version [3]int

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Less orders versions.
func (v Version) Less(o Version) bool {
	for i := 0; i < 3; i++ {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// ParseVersion reads "v20.11.1", "20.11", "8.3.6-1ubuntu1" and similar.
// Missing parts are zero.
func ParseVersion(s string) (Version, bool) {
	v, n, ok := parsePartial(s)
	return v, ok && n > 0
}

// parsePartial parses up to three numeric parts and reports how many were
// given; "x", "X" and "*" end the version (a wildcard).
func parsePartial(s string) (Version, int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	var v Version
	if s == "" || s == "x" || s == "X" || s == "*" {
		return v, 0, true
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	n := 0
	for i, p := range parts {
		if p == "x" || p == "X" || p == "*" {
			break
		}
		k, err := strconv.Atoi(p)
		if err != nil || k < 0 {
			return v, 0, false
		}
		v[i] = k
		n++
	}
	return v, n, true
}

// Range is a version constraint as written in package.json `engines`,
// .nvmrc or composer.json `require.php`: alternatives separated by `||`
// (or composer's `|`), each a set of comparators that must all hold.
// Supported comparators: `=`, `>`, `>=`, `<`, `<=`, `^`, `~`, `~>`, x-ranges
// (`20`, `20.x`, `8.1.*`), hyphen ranges (`1.2 - 2.3`) and `*`.
type Range struct {
	sets [][]comparator
	raw  string
}

type comparator struct {
	op string // one of = > >= < <=
	v  Version
}

func (r Range) String() string { return r.raw }

// Empty reports whether the range was not given at all.
func (r Range) Empty() bool { return strings.TrimSpace(r.raw) == "" }

// ParseRange parses an npm-style constraint. An empty string matches
// everything.
func ParseRange(s string) (Range, error) { return parseRange(s, false) }

// ParseComposerRange parses a composer.json constraint. It differs from npm
// only in `~1.2`, which composer reads as >=1.2 <2.0.
func ParseComposerRange(s string) (Range, error) { return parseRange(s, true) }

func parseRange(s string, composer bool) (Range, error) {
	r := Range{raw: s}
	norm := strings.ReplaceAll(s, "||", "|")
	for _, alt := range strings.Split(norm, "|") {
		alt = strings.ReplaceAll(alt, ",", " ")
		set, err := parseSet(strings.Fields(alt), composer)
		if err != nil {
			return Range{}, fmt.Errorf("version constraint %q: %w", s, err)
		}
		r.sets = append(r.sets, set)
	}
	return r, nil
}

func parseSet(toks []string, composer bool) ([]comparator, error) {
	var out []comparator
	// Hyphen range: A - B.
	if len(toks) == 3 && toks[1] == "-" {
		lo, _, ok1 := parsePartial(toks[0])
		hi, n, ok2 := parsePartial(toks[2])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("bad hyphen range")
		}
		out = append(out, comparator{">=", lo})
		return append(out, upper(hi, n, "<=")...), nil
	}
	// Join operators written apart from their version (">= 18").
	var joined []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.Trim(t, "<>=^~") == "" && i+1 < len(toks) {
			t += toks[i+1]
			i++
		}
		joined = append(joined, t)
	}
	for _, t := range joined {
		cs, err := parseComparator(t, composer)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}

// upper is the upper bound of a partial version: "20" means < 21.0.0.
func upper(v Version, n int, inclusiveOp string) []comparator {
	switch n {
	case 0:
		return nil
	case 1:
		return []comparator{{"<", Version{v[0] + 1}}}
	case 2:
		return []comparator{{"<", Version{v[0], v[1] + 1}}}
	}
	return []comparator{{inclusiveOp, v}}
}

func parseComparator(t string, composer bool) ([]comparator, error) {
	op := ""
	for _, p := range []string{">=", "<=", "~>", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(t, p) {
			op, t = p, t[len(p):]
			break
		}
	}
	v, n, ok := parsePartial(t)
	if !ok {
		return nil, fmt.Errorf("bad version %q", t)
	}
	switch op {
	case "", "=":
		if n == 0 {
			return nil, nil // * matches everything
		}
		if n == 3 {
			return []comparator{{"=", v}}, nil
		}
		return append([]comparator{{">=", v}}, upper(v, n, "<=")...), nil
	case ">", ">=", "<", "<=":
		if n < 3 && (op == ">" || op == "<=") {
			// ">20" means >= 21; "<=20" means < 21.
			b := upper(v, n, op)
			if op == ">" {
				return []comparator{{">=", b[0].v}}, nil
			}
			return b, nil
		}
		return []comparator{{op, v}}, nil
	case "^":
		// The left-most non-zero part is fixed.
		var hi Version
		switch {
		case v[0] > 0 || n == 1:
			hi = Version{v[0] + 1}
		case v[1] > 0 || n == 2:
			hi = Version{0, v[1] + 1}
		default:
			hi = Version{0, 0, v[2] + 1}
		}
		return []comparator{{">=", v}, {"<", hi}}, nil
	case "~", "~>":
		// npm: ~1.2.3 is < 1.3.0, ~1 is < 2.0.0. Composer's ~1.2 is < 2.0.0.
		hi := Version{v[0], v[1] + 1}
		if n == 1 || (n == 2 && (composer || op == "~>")) {
			hi = Version{v[0] + 1}
		}
		return []comparator{{">=", v}, {"<", hi}}, nil
	}
	return nil, fmt.Errorf("bad operator in %q", t)
}

// Match reports whether v satisfies the range.
func (r Range) Match(v Version) bool {
	if len(r.sets) == 0 {
		return true
	}
	for _, set := range r.sets {
		ok := true
		for _, c := range set {
			if !c.match(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (c comparator) match(v Version) bool {
	switch c.op {
	case "=":
		return v == c.v
	case ">":
		return c.v.Less(v)
	case ">=":
		return !v.Less(c.v)
	case "<":
		return v.Less(c.v)
	case "<=":
		return !c.v.Less(v)
	}
	return false
}
