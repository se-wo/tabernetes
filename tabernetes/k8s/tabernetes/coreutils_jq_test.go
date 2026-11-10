package main

import (
	"strings"
	"testing"
)

const jqPods = `{"items":[{"metadata":{"name":"a"},"n":1},{"metadata":{"name":"b"},"n":2}]}`

const jqPod = `apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  replicas: 1
  containers:
    - name: c
      image: nginx:1.27
`

func TestJQ(t *testing.T) {
	for _, tc := range []struct {
		script, want string
		code         int
	}{
		{`echo '` + jqPods + `' | jq -r '.items[].metadata.name'`, "a\nb\n", 0},
		{`echo '` + jqPods + `' | jq '.items[0].metadata'`, "{\n  \"name\": \"a\"\n}\n", 0},
		{`echo '{"a":[1,2]}' | jq -c .`, "{\"a\":[1,2]}\n", 0},
		{`jq -n --arg x hi '$x'`, "\"hi\"\n", 0},
		{`jq -n --argjson x '{"a":1}' '$x.a'`, "1\n", 0},
		{`jq -n '1+1'`, "2\n", 0},
		{`printf '1 2 3' | jq -s length`, "3\n", 0},
		{`printf '1 2 3' | jq -n '[inputs]' -c`, "[1,2,3]\n", 0},
		{`printf 'a\nb\n' | jq -R .`, "\"a\"\n\"b\"\n", 0},
		{`printf 'a\nb\n' | jq -Rs .`, "\"a\\nb\\n\"\n", 0},
		{`echo false | jq -e .`, "false\n", 1},
		{`echo 1 | jq -e 'empty'`, "", 4},
		{`echo 1 | jq -e .`, "1\n", 0},
		{`jq -n '1 +'`, "compile error", 3},
		{`echo '{' | jq .`, "jq: error", 2},
		{`echo '"a"' | jq -j .`, "a", 0},
		{`echo '[1]' | jq --tab .`, "[\n\t1\n]\n", 0},
		{`echo '[1]' | jq --indent 1 .`, "[\n 1\n]\n", 0},
		{`echo '1' | jq '.a'`, "expected an object", 5},
		{`FOO=bar jq -n '$ENV.FOO'`, "", 0},
		{`export FOO=bar; jq -n '$ENV.FOO, env.FOO'`, "\"bar\"\n\"bar\"\n", 0},
		{`jq -n -c '$ARGS.positional' --args a b`, "[\"a\",\"b\"]\n", 0},
		{`jq -n -c '$ARGS.positional' --jsonargs 1 '{"a":2}'`, "[1,{\"a\":2}]\n", 0},
		{`echo '{"a":1}' > f.json; jq .a f.json`, "1\n", 0},
		{`jq .a nofile.json`, "Could not open", 2},
	} {
		got, code := sh(t, tc.script)
		if code != tc.code || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q (%d), want %q (%d)", tc.script, got, code, tc.want, tc.code)
		}
	}
}

func TestYQ(t *testing.T) {
	w := "cat > pod.yaml <<'EOF'\n" + jqPod + "EOF\n"
	for _, tc := range []struct {
		script, want string
		code         int
	}{
		{w + `yq '.spec.containers[0].image' pod.yaml`, "nginx:1.27\n", 0},
		{w + `yq '.metadata.name' < pod.yaml`, "web\n", 0},
		{w + `yq e '.metadata.name' pod.yaml`, "web\n", 0},
		{w + `yq -o json '.metadata' pod.yaml`, "{\n  \"name\": \"web\"\n}\n", 0},
		{w + `yq -o=json -I 4 '.metadata' pod.yaml`, "{\n    \"name\": \"web\"\n}\n", 0},
		{w + `yq '.spec.containers' pod.yaml`, "- image: nginx:1.27\n  name: c\n", 0},
		{w + `yq -i '.spec.replicas = 3' pod.yaml; yq '.spec.replicas' pod.yaml; cat pod.yaml`, "3\napiVersion: v1\n", 0},
		{w + `yq -e '.nothing' pod.yaml`, "null\n", 1},
		{"printf 'a: 1\\n---\\na: 2\\n' | yq '.a'", "1\n---\n2\n", 0},
		{"printf 'a: 1\\n---\\na: 2\\n' > m.yaml; yq '.a' m.yaml", "1\n---\n2\n", 0},
		{`printf 'a: x\n' | yq '.a'`, "x\n", 0},
		{`X=hello yq -n 'strenv(X)'`, "", 0},
		{`export X=hello; yq -n 'strenv(X)'`, "hello\n", 0},
		{`export X=3; yq -n 'env(X) + 1'`, "4\n", 0},
		{`yq -n '1 +'`, "Error", 1},
		{`printf 'a: [1, 2]\n' | yq -o json -I 0 '.'`, "{\"a\":[1,2]}\n", 0},
		{`printf 'a: 1\n' | yq ea '.a'`, "1\n", 0},
		{`yq -n '{"a": {"b": [1]}}'`, "a:\n  b:\n    - 1\n", 0},
	} {
		got, code := sh(t, tc.script)
		if code != tc.code || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q (%d), want %q (%d)", tc.script, got, code, tc.want, tc.code)
		}
	}
}
