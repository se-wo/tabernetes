package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAwk(t *testing.T) {
	pods := "NAME READY STATUS\\nweb-1 1/1 Running\\nweb-2 0/1 Pending\\nweb-3 1/1 Running\\n"
	for _, c := range []struct {
		script, want string
		code         int
	}{
		{`echo "a b c" | awk '{print $2}'`, "b\n", 0},
		{`echo "a:b:c" | awk -F: '{print $3}'`, "c\n", 0},
		{`echo "a:b:c" | awk -F : '{print $1}'`, "a\n", 0},
		{`awk -v x=1 -v y=two 'BEGIN{print x, y}'`, "1 two\n", 0},
		{`printf 'a b\nc d e\n' | awk '{print NR, NF}'`, "1 2\n2 3\n", 0},
		{`printf '1\n2\n3\n' | awk '{s+=$1} END{print s}'`, "6\n", 0},
		{`awk 'BEGIN{printf "%05.1f|%s\n", 3.14159, "x"}'`, "003.1|x\n", 0},
		{`printf '` + pods + `' | awk '/Running/ {c++} END{print c}'`, "2\n", 0},
		{`printf '` + pods + `' | awk 'NR>1 && $3!="Running" {print $1}'`, "web-2\n", 0},
		{`printf '` + pods + `' | awk 'NR>1{split($2,a,"/"); if (a[1]==a[2]) n++} END{print n}'`, "2\n", 0},
		{`echo hi | awk 'BEGIN{exit 3}'`, "", 3},
		{`awk 'BEGIN{print ENVIRON["AWKT"]}'`, "z\n", 0},
		{`awk 'BEGIN{print'`, "", 2},
		{`awk`, "", 2},
		{`awk 'BEGIN{system("echo hi")}'`, "", 2},
	} {
		s := c.script
		if strings.Contains(s, "ENVIRON") {
			s = "export AWKT=z; " + s
		}
		got, code := sh(t, s)
		if code != c.code || (c.code == 0 && got != c.want) {
			t.Errorf("%s: got %q (%d), want %q (%d)", c.script, got, code, c.want, c.code)
		}
	}
	got, code := sh(t, `awk 'BEGIN{print'`)
	if !strings.HasPrefix(got, "awk: ") || code != 2 {
		t.Errorf("syntax error: %q %d", got, code)
	}
}

func TestAwkFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "d.txt"), []byte("x 1\ny 2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "p.awk"), []byte("{s+=$2} END{print s}\n"), 0o644)
	for _, c := range []struct{ script, want string }{
		{`awk '{print FILENAME, $1}' ` + dir + `/d.txt`, dir + "/d.txt x\n" + dir + "/d.txt y\n"},
		{`cd ` + dir + `; awk '{print FILENAME}' d.txt | head -1`, "d.txt\n"},
		{`cd ` + dir + `; awk -f p.awk d.txt`, "3\n"},
		{`cd ` + dir + `; awk '{print $1 > "o.txt"}' d.txt; cat o.txt`, "x\ny\n"},
		{`cd ` + dir + `; awk 'BEGIN{while ((getline l < "d.txt") > 0) n++; print n}'`, "2\n"},
		{`cd ` + dir + `; awk '{print n, $1}' n=7 d.txt`, "7 x\n7 y\n"},
		{`cd ` + dir + `; echo in | awk '{print}' d.txt - `, "x 1\ny 2\nin\n"},
	} {
		if got, _ := sh(t, c.script); got != c.want {
			t.Errorf("%s: got %q, want %q", c.script, got, c.want)
		}
	}
}
