// ocimirror writes WebAssembly images into the OCI mirror of the Tabernetes
// page: a static directory the browser reads same-origin, because ghcr.io
// and Docker Hub send no CORS headers. The layout (read by
// k8s/tabernetes/tabnode_wasi.go):
//
//	<out>/<registry>/<repository>/manifests/<tag>   the image manifest (JSON)
//	<out>/blobs/sha256/<hex>                        manifests, configs and layers
//	<out>/index.json                                the images, for people and tools
//
// Usage:
//
//	ocimirror image    -out DIR -ref REF -wasm FILE [-gzip]   runwasi style: the .wasm in a tar layer, named by the entrypoint
//	ocimirror artifact -out DIR -ref REF -wasm FILE           a wasm OCI artifact: the module is the layer (application/wasm)
//	ocimirror pull     -out DIR -ref REF                      copy an image from its registry (anonymous pull)
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ocimirror image|artifact|pull -out DIR -ref REF [-wasm FILE] [-gzip]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	out := fs.String("out", "", "mirror directory")
	ref := fs.String("ref", "", "image reference")
	wasm := fs.String("wasm", "", "the module")
	gz := fs.Bool("gzip", false, "gzip the layer")
	fs.Parse(os.Args[2:])
	if *out == "" || *ref == "" {
		fs.Usage()
		os.Exit(2)
	}
	m := &mirror{dir: *out}
	r, err := parseRef(*ref)
	if err == nil {
		switch os.Args[1] {
		case "image":
			err = m.image(r, *wasm, *gz)
		case "artifact":
			err = m.artifact(r, *wasm)
		case "pull":
			err = m.pull(r)
		default:
			err = fmt.Errorf("unknown command %q", os.Args[1])
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocimirror %s %s: %v\n", os.Args[1], *ref, err)
		os.Exit(1)
	}
}

type ref struct{ registry, repo, tag string }

func (r ref) String() string { return r.registry + "/" + r.repo + ":" + r.tag }

// parseRef normalizes like Docker (and tabnode_wasi.go) does.
func parseRef(s string) (ref, error) {
	var r ref
	if strings.Contains(s, "@") {
		return r, errors.New("give a tag, not a digest")
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s, r.tag = s[:i], s[i+1:]
	} else {
		r.tag = "latest"
	}
	if first, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.registry, s = first, rest
	} else {
		r.registry = "docker.io"
	}
	if r.registry == "docker.io" && !strings.Contains(s, "/") {
		s = "library/" + s
	}
	r.repo = s
	return r, nil
}

type mirror struct{ dir string }

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *platform         `json:"platform,omitempty"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Manifests     []descriptor      `json:"manifests,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// blob stores b by its digest.
func (m *mirror) blob(mediaType string, b []byte) (descriptor, error) {
	d := descriptor{MediaType: mediaType, Digest: digestOf(b), Size: int64(len(b))}
	p := filepath.Join(m.dir, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return d, err
	}
	return d, os.WriteFile(p, b, 0o644)
}

// tag stores the manifest under its tag and lists it in index.json.
func (m *mirror) tag(r ref, mediaType string, b []byte) error {
	d, err := m.blob(mediaType, b)
	if err != nil {
		return err
	}
	p := filepath.Join(m.dir, r.registry, filepath.FromSlash(r.repo), "manifests", r.tag)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	idx := filepath.Join(m.dir, "index.json")
	var list struct {
		Images map[string]string `json:"images"` // reference -> manifest digest
	}
	if b, err := os.ReadFile(idx); err == nil {
		json.Unmarshal(b, &list)
	}
	if list.Images == nil {
		list.Images = map[string]string{}
	}
	list.Images[r.String()] = d.Digest
	out, _ := json.MarshalIndent(list, "", "  ")
	fmt.Printf("%s %s\n", r, d.Digest)
	return os.WriteFile(idx, append(out, '\n'), 0o644)
}

// image packs the module like runwasi's demo images: a tar layer with the
// .wasm at the root, the entrypoint names it, platform wasip1/wasm.
func (m *mirror) image(r ref, wasmFile string, gz bool) error {
	mod, err := os.ReadFile(wasmFile)
	if err != nil {
		return err
	}
	name := path.Base(r.repo) + ".wasm"
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(mod)), Typeflag: tar.TypeReg, Format: tar.FormatPAX})
	tw.Write(mod)
	if err := tw.Close(); err != nil {
		return err
	}
	diffID := digestOf(layer.Bytes())
	layerType, layerBytes := "application/vnd.oci.image.layer.v1.tar", layer.Bytes()
	if gz {
		var z bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
		zw.Write(layerBytes)
		zw.Close()
		layerType, layerBytes = layerType+"+gzip", z.Bytes()
	}
	cfg, _ := json.Marshal(map[string]any{
		"architecture": "wasm",
		"os":           "wasip1",
		"config":       map[string]any{"Entrypoint": []string{"/" + name}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{diffID}},
	})
	cd, err := m.blob("application/vnd.oci.image.config.v1+json", cfg)
	if err != nil {
		return err
	}
	ld, err := m.blob(layerType, layerBytes)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: cd, Layers: []descriptor{ld}})
	return m.tag(r, "application/vnd.oci.image.manifest.v1+json", b)
}

// artifact packs the module as a wasm OCI artifact (CNCF TAG Runtime spec):
// the module is the only layer.
func (m *mirror) artifact(r ref, wasmFile string) error {
	mod, err := os.ReadFile(wasmFile)
	if err != nil {
		return err
	}
	name := path.Base(r.repo) + ".wasm"
	ld, err := m.blob("application/wasm", mod)
	if err != nil {
		return err
	}
	ld.Annotations = map[string]string{"org.opencontainers.image.title": name}
	cfg, _ := json.Marshal(map[string]any{"architecture": "wasm", "os": "wasip1", "layerDigests": []string{ld.Digest}})
	cd, err := m.blob("application/vnd.wasm.config.v0+json", cfg)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json",
		ArtifactType: "application/vnd.wasm.config.v0+json", Config: cd, Layers: []descriptor{ld}})
	return m.tag(r, "application/vnd.oci.image.manifest.v1+json", b)
}

const accept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// pull copies an image from its registry. Of an index only the wasm
// platform is copied, and its manifest is stored under the tag.
func (m *mirror) pull(r ref) error {
	c := &client{r: r}
	b, mt, err := c.get("manifests/"+r.tag, accept)
	if err != nil {
		return err
	}
	var man manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return err
	}
	if len(man.Manifests) > 0 {
		var pick *descriptor
		for i, d := range man.Manifests {
			if d.Platform != nil && (d.Platform.Architecture == "wasm" || d.Platform.OS == "wasip1" || d.Platform.OS == "wasi") {
				pick = &man.Manifests[i]
				break
			}
		}
		if pick == nil && len(man.Manifests) == 1 {
			pick = &man.Manifests[0]
		}
		if pick == nil {
			return errors.New("the index has no wasm platform")
		}
		if b, mt, err = c.get("manifests/"+pick.Digest, accept); err != nil {
			return err
		}
		man = manifest{}
		if err := json.Unmarshal(b, &man); err != nil {
			return err
		}
	}
	for _, d := range append([]descriptor{man.Config}, man.Layers...) {
		blob, _, err := c.get("blobs/"+d.Digest, "*/*")
		if err != nil {
			return err
		}
		if digestOf(blob) != d.Digest {
			return fmt.Errorf("blob %s: digest mismatch", d.Digest)
		}
		if _, err := m.blob(d.MediaType, blob); err != nil {
			return err
		}
	}
	return m.tag(r, mt, b)
}

type client struct {
	r     ref
	token string
}

func (c *client) get(p, accept string) ([]byte, string, error) {
	host := c.r.registry
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	u := "https://" + host + "/v2/" + c.r.repo + "/" + p
	for try := 0; ; try++ {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Accept", accept)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			if err := c.login(resp.Header.Get("WWW-Authenticate")); err != nil {
				return nil, "", err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		return b, strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]), nil
	}
}

// login gets an anonymous token from the service the 401 names.
func (c *client) login(challenge string) error {
	_, params, _ := strings.Cut(challenge, " ")
	p := map[string]string{}
	for _, kv := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if p["realm"] == "" {
		return fmt.Errorf("no token realm in %q", challenge)
	}
	q := url.Values{"service": {p["service"]}, "scope": {"repository:" + c.r.repo + ":pull"}}
	resp, err := http.Get(p["realm"] + "?" + q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	c.token = t.Token
	if c.token == "" {
		c.token = t.AccessToken
	}
	return nil
}
