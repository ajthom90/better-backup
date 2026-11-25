package version

import (
	"sort"
	"strings"
	"testing"
)

func TestOrder(t *testing.T) {
	// Oldest first.
	order := []string{"0.0.24", "0.1.0-edge.2", "0.1.0-edge.10", "0.1.0-rc.1", "0.1.0-rc.1.edge.3", "0.1.0-rc.2", "0.1.0",
		"0.1.1-edge.1", "v0.1.1", "0.2.0", "0.10.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-beta", "1.0.0"}
	for i, a := range order {
		for j, b := range order {
			if got, want := Newer(a, b), i > j; got != want {
				t.Errorf("Newer(%s, %s) = %v, want %v", a, b, got, want)
			}
		}
	}
	mixed := append([]string{}, order...)
	sort.Slice(mixed, func(i, j int) bool { return mixed[i] > mixed[j] })
	sort.SliceStable(mixed, func(i, j int) bool { return Newer(mixed[j], mixed[i]) })
	if strings.Join(mixed, " ") != strings.Join(order, " ") {
		t.Errorf("sorted: %v", mixed)
	}
}

func TestParse(t *testing.T) {
	for _, bad := range []string{"", "dev", "edge-0d5dd8f", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3-a..b", "ci-abc"} {
		if Valid(bad) {
			t.Errorf("%q parsed", bad)
		}
		if Newer(bad, "0.0.1") || Newer("0.0.1", bad) {
			t.Errorf("%q compares", bad)
		}
	}
	v, ok := Parse("v1.2.3-rc.1+abc")
	if !ok || v.String() != "1.2.3-rc.1+abc" || v.Core() != "1.2.3" || v.Stable() || v.Edge() {
		t.Errorf("got %+v", v)
	}
	// What a .deb calls 1.2.3-edge.4.
	if !Same("1.2.3~edge.4", "v1.2.3-edge.4") || Same("1.2.3", "1.2.3-edge.4") || !Same("1.2.3+a", "1.2.3+b") {
		t.Error("Same is wrong")
	}
	if !Same("dev", "dev") || Same("dev", "1.0.0") {
		t.Error("Same of builds from source is wrong")
	}
}

func TestEdge(t *testing.T) {
	for _, c := range []struct {
		last string
		n    int
		want string
	}{{"0.3.2", 5, "0.3.3-edge.5"}, {"1.0.0-rc.1", 3, "1.0.0-rc.1.edge.3"}} {
		last, _ := Parse(c.last)
		e := EdgeAfter(last, c.n)
		if e.String() != c.want || !e.Edge() || Compare(e, last) <= 0 {
			t.Errorf("EdgeAfter(%s, %d) = %s", c.last, c.n, e)
		}
		for _, kind := range []string{"patch", "minor", "major"} {
			if next, _ := last.Bump(kind); Compare(next, e) <= 0 {
				t.Errorf("%s bumped by %s is %s, not newer than its edge build %s", c.last, kind, next, e)
			}
		}
	}
	if e := EdgeBeforeFirst(142); e.String() != "0.1.0-edge.142" || !Newer(e.String(), "0.0.9999") || !Newer("0.1.0", e.String()) {
		t.Errorf("before the first release: %s", e)
	}
}

func TestBump(t *testing.T) {
	for _, c := range [][3]string{{"1.2.3", "patch", "1.2.4"}, {"1.2.3", "minor", "1.3.0"}, {"1.2.3", "major", "2.0.0"},
		{"1.3.0-rc.1", "patch", "1.3.0"}, {"1.3.0-rc.1", "minor", "1.3.0"}, {"1.3.0-rc.1", "major", "2.0.0"}, {"2.0.0-rc.1", "major", "2.0.0"}} {
		v, _ := Parse(c[0])
		if got, err := v.Bump(c[1]); err != nil || got.String() != c[2] {
			t.Errorf("%s bumped by %s = %s, %v; want %s", c[0], c[1], got, err, c[2])
		}
	}
}

func TestMSIAndTags(t *testing.T) {
	for _, c := range [][3]string{{"1.4.2", "1.4.2", "1.4.2 1.4 1 latest"}, {"0.4.2", "0.4.2", "0.4.2 0.4 latest"},
		{"1.5.0-rc.1", "1.5.0.1", "1.5.0-rc.1"}, {"1.4.3-edge.12", "1.4.3.12", "1.4.3-edge.12 edge"}, {"1.4.3-edge.70000", "1.4.3.65535", "1.4.3-edge.70000 edge"}} {
		v, _ := Parse(c[0])
		if v.MSI() != c[1] {
			t.Errorf("MSI of %s = %s, want %s", c[0], v.MSI(), c[1])
		}
		if got := strings.Join(v.DockerTags(true, true, true), " "); got != c[2] {
			t.Errorf("tags of %s = %s, want %s", c[0], got, c[2])
		}
	}
	v, _ := Parse("1.4.9")
	if got := strings.Join(v.DockerTags(true, false, false), " "); got != "1.4.9 1.4" {
		t.Errorf("tags of an older line: %s", got)
	}
}
