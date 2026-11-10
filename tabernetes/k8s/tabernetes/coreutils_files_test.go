package main

import (
	"strings"
	"testing"
)

func TestFilesTools(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		// cp
		{"echo hi >a; cp a b; cat b", "hi\n"},
		{"echo hi >a; mkdir d; cp a d; ls d", "a\n"},
		{"echo 1 >a; echo 2 >b; mkdir d; cp a b d/; ls d", "a\nb\n"},
		{"mkdir -p s/x; echo 1 >s/x/f; cp -r s t; cat t/x/f", "1\n"},
		{"mkdir -p s/x; echo 1 >s/x/f; mkdir t; cp -R s t; cat t/s/x/f", "1\n"},
		{"mkdir s; cp s t", "cp: -r not specified; omitting directory 's'\n"},
		{"cp nope b", "cp: cannot stat 'nope': No such file or directory\n"},
		{"echo hi >a; cp -v a b", "'a' -> 'b'\n"},
		{"echo 1 >a; echo 2 >b; cp -n a b; cat b", "2\n"},
		{"echo 1 >a; chmod 600 a; cp -a a b; stat -c %a b", "600\n"},
		// mv
		{"echo hi >a; mv a b; cat b; ls", "hi\nb\n"},
		{"echo hi >a; mkdir d; mv a d; find d -type f", "d/a\n"},
		{"mkdir -p s/x; mv s t; find t -type d", "t\nt/x\n"},
		{"echo 1 >a; echo 2 >b; mv -n a b; cat b", "2\n"},
		{"echo 1 >a; mv -v a b", "renamed 'a' -> 'b'\n"},
		{"mv nope b", "mv: cannot stat 'nope': No such file or directory\n"},
		// ln
		{"echo hi >a; ln a b; cat b; stat -c %h a", "hi\n2\n"},
		{"echo hi >a; ln -s a b; cat b; readlink b; stat -c %F b", "hi\na\nsymbolic link\n"},
		{"echo hi >a; ln -s a b; stat -c %N b", "'b' -> 'a'\n"},
		{"echo 1 >a; echo 2 >b; ln -sf a b; cat b", "1\n"},
		{"echo 1 >a; echo 2 >b; ln -s a b", "ln: failed to create symbolic link 'b': File exists\n"},
		{"echo 1 >a; mkdir d; ln -s ../a d/; readlink d/a", "../a\n"},
		// chmod
		{"touch a; chmod 644 a; stat -c '%a %A' a", "644 -rw-r--r--\n"},
		{"touch a; chmod 0755 a; stat -c %a a", "755\n"},
		{"touch a; chmod 600 a; chmod u+x,g=r a; stat -c %a a", "740\n"},
		{"touch a; chmod 666 a; chmod go-w a; stat -c %a a", "644\n"},
		{"touch a; chmod 000 a; chmod a=r a; stat -c %a a", "444\n"},
		{"touch a; chmod 600 a; chmod +x a; stat -c %a a", "711\n"},
		{"touch a; chmod 777 a; chmod u=rwx,g=rx,o= a; stat -c %a a", "750\n"},
		{"touch a; chmod 644 a; chmod -w a; stat -c %a a", "444\n"},
		{"mkdir -p d/e; touch d/e/f; chmod -R 700 d; stat -c %a d d/e d/e/f", "700\n700\n700\n"},
		{"touch a; chmod 4755 a; stat -c '%a %A' a", "4755 -rwsr-xr-x\n"},
		{"touch a; chmod bogus a", "chmod: invalid mode: 'bogus'\n"},
		{"chmod 644 nope", "chmod: cannot access 'nope': No such file or directory\n"},
		// stat
		{"printf 'abcdef' >a; stat -c '%n %s %F' a", "a 6 regular file\n"},
		{"touch a; stat -c %F a", "regular empty file\n"},
		{"mkdir d; stat -c '%F' d", "directory\n"},
		{"touch a; stat --printf '%n\\n%a\\n' a", "a\n644\n"},
		{"stat nope", "stat: cannot statx 'nope': No such file or directory\n"},
		{"echo hi >a; ln -s a l; stat -c %F l; stat -L -c %F l", "symbolic link\nregular file\n"},
		// find
		{"mkdir -p a/b; touch a/x.txt a/b/y.go; find . -name '*.go'", "./a/b/y.go\n"},
		{"mkdir -p a/b; touch a/x.txt a/b/y.go; find a", "a\na/b\na/b/y.go\na/x.txt\n"},
		{"mkdir -p a/b; touch a/x.txt a/b/y.go; find a -type d", "a\na/b\n"},
		{"mkdir -p a/b; touch a/x.txt a/b/y.go; find a -type f -name '*.txt'", "a/x.txt\n"},
		{"mkdir -p a/b; touch a/X.txt; find a -iname 'x.TXT'", "a/X.txt\n"},
		{"mkdir -p a/b; touch a/b/y; find a -path '*/b/*'", "a/b/y\n"},
		{"mkdir -p a/b/c; find a -maxdepth 1", "a\na/b\n"},
		{"mkdir -p a/b/c; find a -mindepth 2", "a/b/c\n"},
		{"mkdir a; touch a/f; echo 1 >a/g; find a -type f -empty", "a/f\n"},
		{"mkdir a; touch a/f; seq 1000 >a/g; find a -size -1k -type f", "a/f\n"},
		{"mkdir a; touch a/f a/g; find a -type f -not -name f", "a/g\n"},
		{"mkdir a; touch a/f a/g a/h; find a -type f \\( -name f -o -name g \\)", "a/f\na/g\n"},
		{"mkdir a; touch a/f a/g; find a -type f ! -name f -print", "a/g\n"},
		{"mkdir a; touch a/f; find a -type f -print0", "a/f\x00"},
		{"mkdir a; touch a/f; find a -mtime 0 -type f", "a/f\n"},
		{"mkdir a; touch a/f; find a -mmin -5 -type f", "a/f\n"},
		{"mkdir a; touch a/f; find a -mtime +1 -type f", ""},
		{"mkdir -p a/b; touch a/b/f; find a -delete; ls", ""},
		{"mkdir a; echo x >a/f; echo y >a/g; find a -type f -exec cat {} \\;", "x\ny\n"},
		{"mkdir a; echo x >a/f; echo y >a/g; find a -type f -exec stat -c %n {} +", "a/f\na/g\n"},
		{"mkdir a; touch a/f; find a -type f -exec stat -c '%n!' {} \\;", "a/f!\n"},
		{"find nope", "find: 'nope': No such file or directory\n"},
		{"find . -bogus", "find: unknown predicate `-bogus'\n"},
		// diff
		{"printf '1\\n2\\n3\\n' >a; printf '1\\n2\\n3\\n' >b; diff a b; echo $?", "0\n"},
		{"printf '1\\n2\\n3\\n' >a; printf '1\\nX\\n3\\n' >b; diff a b; echo $?", "2c2\n< 2\n---\n> X\n1\n"},
		{"printf '1\\n2\\n' >a; printf '1\\n2\\n3\\n' >b; diff a b", "2a3\n> 3\n"},
		{"printf '1\\n2\\n3\\n' >a; printf '1\\n3\\n' >b; diff a b", "2d1\n< 2\n"},
		{"printf '1\\n2\\n3\\n4\\n5\\n6\\n7\\n8\\n' >a; printf '1\\n2\\n3\\n4\\nX\\n6\\n7\\n8\\n' >b; diff -u a b | tail -n +3", "@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+X\n 6\n 7\n 8\n"},
		{"printf 'a\\nb\\nc\\n' >a; printf 'a\\nb\\nc\\nd\\n' >b; diff -u a b | tail -n +3", "@@ -1,3 +1,4 @@\n a\n b\n c\n+d\n"},
		{"printf 'a\\nb\\nc\\n' >a; printf 'a\\nc\\n' >b; diff -U1 a b | tail -n +3", "@@ -1,3 +1,2 @@\n a\n-b\n c\n"},
		{"printf 'a\\n' >a; printf 'b\\n' >b; diff -u a b | grep -c '^--- a'", "1\n"},
		{"printf 'a\\n' >a; printf 'b\\n' >b; diff -q a b; echo $?", "Files a and b differ\n1\n"},
		{"printf 'a' >a; printf 'a\\n' >b; diff a b", "1c1\n< a\n\\ No newline at end of file\n---\n> a\n"},
		{"printf 'a\\n' >a; printf 'a\\n' | diff a -; echo $?", "0\n"},
		{"diff nope nope2; echo $?", "diff: nope: No such file or directory\n2\n"},
		{"mkdir x y; touch x/only x/same y/same; mkdir x/sub y/sub; diff x y; echo $?", "Only in x: only\nCommon subdirectories: x/sub and y/sub\n1\n"},
		{"mkdir x y; echo 1 >x/f; echo 2 >y/f; diff -rq x y", "Files x/f and y/f differ\n"},
		{"mkdir x y; echo 1 >x/f; echo 2 >y/f; diff -r x y", "diff -r x/f y/f\n1c1\n< 1\n---\n> 2\n"},
		// small tools
		{"mkdir -p a/b/c; rmdir a/b/c; ls a/b", ""},
		{"mkdir -p a/b/c; rmdir -p a/b/c; ls", ""},
		{"mkdir a; touch a/f; rmdir a", "rmdir: failed to remove 'a': Directory not empty\n"},
		{"basename /a/b/c.txt", "c.txt\n"},
		{"basename /a/b/c.txt .txt", "c\n"},
		{"basename /a/b/", "b\n"},
		{"dirname /a/b/c.txt a/b c / x", "/a/b\na\n.\n/\n.\n"},
		{"mkdir a; realpath a/../a | grep -c '/a$'", "1\n"},
		{"echo hi >a; ln -s a l; readlink -f l | grep -c '/a$'", "1\n"},
		{"readlink nope", ""},
		{"echo hi >a; ln -s a l; readlink l", "a\n"},
		{"d=$(mktemp -d); test -d $d && echo dir; case $d in /tmp/tmp.*) echo ok;; esac; rmdir $d", "dir\nok\n"},
		{"f=$(mktemp XXXXXX); test -f $f && echo file; echo ${#f}", "file\n6\n"},
		{"mktemp abc", "mktemp: too few X's in template 'abc'\n"},
		// checksums
		{"printf abc | sha256sum", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  -\n"},
		{"printf abc >a; sha256sum a", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  a\n"},
		{"printf abc >a; md5sum a", "900150983cd24fb0d6963f7d28e17f72  a\n"},
		{"printf abc >a; sha1sum a", "a9993e364706816aba3e25717850c26c9cd0d89d  a\n"},
		{"printf abc >a; sha512sum a | grep -c '^ddaf35a193617aba'", "1\n"},
		{"printf abc >a; sha256sum a >s; sha256sum -c s", "a: OK\n"},
		{"printf abc >a; sha256sum a >s; printf x >a; sha256sum -c s; echo $?", "a: FAILED\nsha256sum: WARNING: 1 computed checksum(s) did NOT match\n1\n"},
		{"sha256sum nope", "sha256sum: nope: No such file or directory\n"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, _ := sh(t, tc.script)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilesChown(t *testing.T) {
	got, code := sh(t, "touch a; chown 1234:5678 a; stat -c '%u %g' a")
	if code != 0 || strings.Contains(got, "chown:") {
		t.Skipf("chown not permitted here: %q", got)
	}
	for _, tc := range []struct{ script, want string }{
		{"touch a; chown 1234:5678 a; stat -c '%u %g' a", "1234 5678\n"},
		{"touch a; chown 1234 a; stat -c '%u' a", "1234\n"},
		{"touch a; chown :99 a; stat -c '%g' a", "99\n"},
		{"touch a; chgrp 77 a; stat -c '%g' a", "77\n"},
		{"mkdir d; touch d/f; chown -R 42 d; stat -c %u d/f", "42\n"},
		{"touch a; ln -s a l; chown -h 55 l; stat -c %u l; stat -c %u a", "55\n0\n"},
		{"touch a; chown nosuchuser_x a", "chown: invalid user: 'nosuchuser_x'\n"},
		{"touch a; chgrp nosuchgroup_x a", "chgrp: invalid group: 'nosuchgroup_x'\n"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			if got, _ := sh(t, tc.script); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilesStatDefault(t *testing.T) {
	got, code := sh(t, "printf abcdef >a; chmod 644 a; stat a")
	if code != 0 {
		t.Fatal(got)
	}
	for _, want := range []string{"  File: a\n  Size: 6 ", "regular file", "Access: (0644/-rw-r--r--)  Uid: (", "Modify: ", "Change: ", " Birth: "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestFilesExitCodes(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{
		{"echo a >a; echo a >b; diff a b", 0},
		{"echo a >a; echo b >b; diff a b", 1},
		{"diff a nope", 2},
		{"cp nope b", 1},
		{"find nope", 1},
		{"touch a; find a -name a", 0},
	} {
		if _, code := sh(t, tc.script); code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.script, code, tc.want)
		}
	}
}
