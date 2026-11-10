package main

// Image pulls of the tab node. Nothing is pulled for native containers, but
// the node knows what a real node would find: the built-in images, the
// official images of Docker Hub and, for the built-in repositories, the
// shape of a tag. A wrong image therefore fails like on a real cluster
// (ErrImagePull, ImagePullBackOff). Images it cannot judge always "pull".

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
)

// The delays of the pull back-off, variables for tests.
var (
	pullErrorWait   = 2 * time.Second  // ErrImagePull turns into ImagePullBackOff
	pullBackOffBase = 10 * time.Second // first wait, doubles up to pullBackOffMax
	pullBackOffMax  = 5 * time.Minute
	pullTimeBase    = 300 * time.Millisecond // a pull with policy Always takes 1 to 3 times this
)

const dockerIO = "docker.io"

// fullRef normalises an image reference: registry, repository (with
// "library/" on Docker Hub), tag ("latest" without tag and digest) and
// digest. It is lower case.
func fullRef(ref string) (registry, repo, tag, digest string) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref, tag = ref[:i], ref[i+1:]
	}
	registry = dockerIO
	if first, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		registry, ref = first, rest
	}
	switch registry {
	case "index.docker.io", "registry-1.docker.io":
		registry = dockerIO
	}
	repo = ref
	if registry == dockerIO && repo != "" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return
}

func canonicalRef(registry, repo, tag, digest string) string {
	s := registry + "/" + repo
	if tag != "" {
		s += ":" + tag
	}
	if digest != "" {
		s += "@" + digest
	}
	return s
}

// defaultPullPolicy is what the API server defaults to.
func defaultPullPolicy(image string) v1.PullPolicy {
	_, _, tag, digest := fullRef(image)
	if tag == "latest" || (tag == "" && digest == "") {
		return v1.PullAlways
	}
	return v1.PullIfNotPresent
}

// dockerOfficialImages are all names that were ever in library/ of
// github.com/docker-library/official-images (commit 0088442ac889): the
// current images and the deprecated ones that can still be pulled.
// Regenerate in a clone of that repository with
//
//	git ls-tree --name-only HEAD library/; git log --diff-filter=D --name-only --format= -- library/
//
// minus MAINTAINERS.
var dockerOfficialImages = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`adminer adoptopenjdk aerospike almalinux alpine alt amazoncorretto amazonlinux api-firewall arangodb archlinux
azul-zulu backdrop bash bonita buildpack-deps busybox caddy cassandra celery centos chronograf cirros
clearlinux clefos clickhouse clojure composer consul convertigo couchbase couchdb couchdb-nouveau crate crux
dart debian django docker docker-dev drupal eclipse-mosquitto eclipse-temurin eggdrop elasticsearch elixir
emqx erlang espocrm euleros express-gateway fedora flink fluentd friendica fsharp gazebo gcc geonetwork ghost
glassfish golang gradle groovy haproxy haskell haxe hello-seattle hello-world hipache hitch hola-mundo httpd
hylang ibm-semeru-runtimes ibmjava influxdb iojs irssi java jenkins jetty jobber joomla jruby julia
kaazing-gateway kapacitor kibana known kong krakend lightstreamer lightstreamer-server liquibase logstash
mageia mariadb matomo maven mediawiki memcached mongo mongo-express monica mono mosquitto mysql nats
nats-streaming neo4j neurodebian nextcloud nginx nim node notary notary-server notary-signer nuxeo odoo
open-liberty openjdk opensuse oraclelinux orientdb owncloud percona perl photon php php-zendserver phpmyadmin
piwik plone postfixadmin postgres pypy python r-base rabbitmq rails rakudo-star rapidoid redis redmine
registry rethinkdb rocket.chat rockylinux ros ruby rust sapmachine satosa sentry silverpeas sl solr sonarqube
sourcemage spark spiped storm swarm swift swipl teamspeak telegraf thrift tomcat tomee traefik ubuntu
ubuntu-debootstrap ubuntu-upstart unit varnish vault websphere-liberty wordpress xwiki yourls zend-php znc
zookeeper`) {
		m[n] = true
	}
	return m
}()

// Namespaces with many unrelated repositories: a typo cannot be told from
// another project.
var openNamespaces = map[string]bool{"bitnami": true, "rancher": true, "opensuse": true, "library": true}

func defRegistry(d *imageDef) string {
	if d.registry == "" {
		return dockerIO
	}
	return d.registry
}

// defRepo is the repository path of a built-in repo on its registry.
func defRepo(d *imageDef, r string) string {
	if defRegistry(d) == dockerIO && !strings.Contains(r, "/") {
		return "library/" + r
	}
	return r
}

// builtinRepo says whether registry/repo is exactly a repository of a
// built-in image.
func builtinRepo(registry, repo string) bool {
	for _, d := range imageDefs {
		if defRegistry(d) != registry {
			continue
		}
		for _, r := range d.repos {
			if defRepo(d, r) == repo {
				return true
			}
		}
	}
	return false
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// typoOfBuiltin says whether the repository sits in the namespace of a
// built-in repository, is none itself and is named almost like one.
func typoOfBuiltin(registry, repo string) bool {
	ns, last := path.Split(repo)
	ns = strings.TrimSuffix(ns, "/")
	if ns == "" || openNamespaces[strings.SplitN(ns, "/", 2)[0]] || builtinRepo(registry, repo) {
		return false
	}
	for _, d := range imageDefs {
		if defRegistry(d) != registry {
			continue
		}
		for _, r := range d.repos {
			rns, rlast := path.Split(defRepo(d, r))
			if strings.TrimSuffix(rns, "/") == ns && levenshtein(last, rlast) <= 2 {
				return true
			}
		}
	}
	return false
}

var (
	tagVersion    = regexp.MustCompile(`^v?\d+([._]\d+)*$`)
	tagPreRelease = regexp.MustCompile(`^(rc|beta|alpha)[.]?\d*$`)
	tagRevision   = regexp.MustCompile(`^r\d+$`) // bitnami: debian-12-r3
)

var tagWords = strings.Fields(`latest alpine slim bookworm bullseye buster stretch jessie trixie forky sid stable
oldstable testing unstable jammy noble focal bionic xenial trusty oracular plucky questing rawhide mainline perl
otel musl glibc uclibc unprivileged lts current edge minimal jdk jre temurin corretto openjdk oracle apache fpm cli
zts management innovation windowsservercore nanoserver servercore ltsc stream ubi el debian ubuntu centos php
python node full dev git core nonroot distroless root community enterprise sapmachine semeru graalvm
amazoncorretto liberica zulu plain text devel backports experimental init fips base multilib curl linux raspi`)

// validTag says whether a tag looks like a tag of the official images.
func validTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, tok := range strings.Split(tag, "-") {
		if !validTagToken(tok) {
			return false
		}
	}
	return true
}

func validTagToken(tok string) bool {
	if tagVersion.MatchString(tok) || tagPreRelease.MatchString(tok) || tagRevision.MatchString(tok) {
		return true
	}
	for _, w := range tagWords {
		if rest, ok := strings.CutPrefix(tok, w); ok && (rest == "" || tagVersion.MatchString(rest)) {
			return true
		}
	}
	return false
}

// checkImageRef decides whether a pull of the image succeeds. The error is
// the message of containerd.
func checkImageRef(ref string) error {
	registry, repo, tag, digest := fullRef(ref)
	if repo == "" {
		return nil
	}
	canon := canonicalRef(registry, repo, tag, digest)
	denied := fmt.Errorf("rpc error: code = Unknown desc = failed to pull and unpack image %q: failed to resolve reference %q: pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed", canon, canon)

	known := builtinRepo(registry, repo)
	if name, ok := strings.CutPrefix(repo, "library/"); ok && registry == dockerIO && !strings.Contains(name, "/") {
		if !known && !dockerOfficialImages[name] {
			return denied
		}
	} else if typoOfBuiltin(registry, repo) {
		return denied
	}
	// Only the tags of the built-in images are checked: the official
	// images have too many odd tags (traefik:mimolette, node:iron).
	if known && digest == "" && !validTag(tag) {
		return fmt.Errorf("rpc error: code = NotFound desc = failed to pull and unpack image %q: failed to resolve reference %q: %s: not found", canon, canon, canon)
	}
	return nil
}

// pullWithBackOff calls pull until it succeeds, like the kubelet: a failure
// is ErrImagePull, then ImagePullBackOff with a growing delay. A failure
// for which retry says false is returned at once. It returns only on
// success, such an error or the end of ctx.
func (c *ctr) pullWithBackOff(ctx context.Context, pull func() error, retry func(error) bool) error {
	delay := pullBackOffBase
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			c.w.ctrEvent(c, v1.EventTypeNormal, "Pulling", "Pulling image %q", c.spec.Image)
		}
		err := pull()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if retry != nil && !retry(err) {
			return err
		}
		msg := err.Error()
		c.setWaiting("ErrImagePull", msg)
		c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Failed to pull image %q: %s", c.spec.Image, msg)
		c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: ErrImagePull")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pullErrorWait):
		}
		c.setWaiting("ImagePullBackOff", fmt.Sprintf("Back-off pulling image %q: ErrImagePull: %s", c.spec.Image, msg))
		c.w.ctrEvent(c, v1.EventTypeNormal, "BackOff", "Back-off pulling image %q", c.spec.Image)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, pullBackOffMax)
	}
}

// pullSize is the size shown for a pulled image: the real one for a
// built-in image, else a stable number made from the reference.
func pullSize(ref string) int64 {
	if d, _ := findImage(ref); d != nil {
		return d.size
	}
	sum := sha256.Sum256([]byte(ref))
	return 5_000_000 + int64(binary.BigEndian.Uint64(sum[:8])%145_000_000)
}

// pullTime is how long a pull takes: 300 to 900ms, stable per reference.
func pullTime(ref string) time.Duration {
	sum := sha256.Sum256([]byte("pull " + ref))
	return pullTimeBase + time.Duration(binary.BigEndian.Uint16(sum[:2])%600)*pullTimeBase/300
}

// pullNative "pulls" the image of a native container the way the kubelet
// does before every start. It returns false when ctx ended.
func (c *ctr) pullNative(ctx context.Context) bool {
	image := c.spec.Image
	policy := c.spec.ImagePullPolicy
	if policy == "" {
		policy = defaultPullPolicy(image)
	}
	present := func() {
		c.w.ctrEvent(c, v1.EventTypeNormal, "Pulled", "Container image %q already present on machine and can be accessed by the pod", image)
	}
	check := func() error { return checkImageRef(image) }
	switch policy {
	case v1.PullNever:
		if checkImageRef(image) != nil {
			msg := fmt.Sprintf("Container image %q is not present with pull policy of Never", image)
			c.setWaiting("ErrImageNeverPull", msg)
			c.w.ctrEvent(c, v1.EventTypeWarning, "ErrImageNeverPull", "%s", msg)
			c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: ErrImageNeverPull")
			<-ctx.Done()
			return false
		}
		present()
	case v1.PullIfNotPresent:
		if checkImageRef(image) == nil {
			present()
			return true
		}
		c.w.ctrEvent(c, v1.EventTypeNormal, "Pulling", "Pulling image %q", image)
		return c.pullWithBackOff(ctx, check, nil) == nil
	default:
		start := time.Now()
		c.w.ctrEvent(c, v1.EventTypeNormal, "Pulling", "Pulling image %q", image)
		if checkImageRef(image) != nil {
			return c.pullWithBackOff(ctx, check, nil) == nil
		}
		// A pull of an image the node has takes a moment for the manifest.
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pullTime(image)):
		}
		d := time.Since(start).Truncate(time.Millisecond)
		c.w.ctrEvent(c, v1.EventTypeNormal, "Pulled", "Successfully pulled image %q in %v (%v including waiting). Image size: %v bytes.", image, d, d, pullSize(image))
	}
	return ctx.Err() == nil
}

// pullImage pulls the image of a native container and checks that it has a
// command. false: ctx ended. While the container waits for its image (a
// pull back-off, ErrImageNeverPull, CreateContainerError), a new image in
// the pod spec (kubectl set image, kubectl edit) starts over with it, like
// on a real node.
func (c *ctr) pullImage(ctx context.Context) bool {
	c.w.mu.Lock()
	if c.newImage == nil {
		c.newImage = make(chan struct{}, 1)
	}
	changed := c.newImage
	c.w.mu.Unlock()
	for {
		wait, stop := context.WithCancel(ctx)
		go func() {
			select {
			case <-changed:
				stop()
			case <-wait.Done():
			}
		}()
		if c.pullNative(wait) {
			c.w.mu.Lock()
			argv := c.argv
			c.w.mu.Unlock()
			if len(argv) > 0 {
				stop()
				return true
			}
			msg := fmt.Sprintf("failed to generate container %q spec: failed to generate spec: no command specified (image %q is not built into node tab: give the container a command, it runs in a POSIX shell)", hexID(), c.spec.Image)
			c.setWaiting("CreateContainerError", msg)
			c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %s", msg)
			<-wait.Done()
		}
		stop()
		if ctx.Err() != nil {
			return false
		}
		// A new image: the command, environment and files follow it.
		for {
			err := c.prepare()
			if err == nil {
				break
			}
			c.setWaiting("CreateContainerConfigError", err.Error())
			c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
			select {
			case <-ctx.Done():
				return false
			case <-changed:
			}
		}
	}
}

// waitsForImage says whether the container waits for its image, so that a
// new image in the spec applies right away. w.mu is held.
func (c *ctr) waitsForImage() bool {
	if c.ephemeral || c.state.Waiting == nil {
		return false
	}
	switch c.state.Waiting.Reason {
	case "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "CreateContainerError":
		return true
	}
	return false
}

// imageKill is a running container that is killed for a new image.
type imageKill struct {
	c    *ctr
	proc *process
}

// imagesChangedLocked takes the new images of the containers from the updated
// pod. A container that waits for its image starts over with the new one at
// once; a running one is returned to be killed and started again (the caller
// passes it to restartForNewImage once w.mu is released). w.mu is held.
func (w *podWorker) imagesChangedLocked(pod *v1.Pod) []imageKill {
	specs := map[string]v1.Container{}
	for _, s := range pod.Spec.InitContainers {
		specs[s.Name] = s
	}
	for _, s := range pod.Spec.Containers {
		specs[s.Name] = s
	}
	var kill []imageKill
	for _, c := range w.ctrs {
		s, ok := specs[c.spec.Name]
		if !ok || c.ephemeral || c.finished || s.Image == c.spec.Image {
			continue
		}
		switch {
		case c.waitsForImage():
			c.spec.Image, c.spec.ImagePullPolicy = s.Image, s.ImagePullPolicy
			if c.newImage == nil {
				c.newImage = make(chan struct{}, 1)
			}
			select {
			case c.newImage <- struct{}{}:
			default:
			}
		case c.proc != nil && (!c.init || c.sidecar):
			c.spec.Image, c.spec.ImagePullPolicy = s.Image, s.ImagePullPolicy
			c.specRestart, c.reprepare = true, true
			kill = append(kill, imageKill{c, c.proc})
		default:
			c.spec.Image, c.spec.ImagePullPolicy = s.Image, s.ImagePullPolicy
			c.reprepare = true
		}
	}
	return kill
}

// restartForNewImage kills the running containers whose image changed, like
// the kubelet: the Killing event, the preStop hook, the kill. The supervisor
// starts them again with the new image.
func (w *podWorker) restartForNewImage(kill []imageKill) {
	if len(kill) == 0 {
		return
	}
	pod, _ := w.podInfo()
	by := time.Now().Add(killGrace(pod, nil))
	for _, k := range kill {
		go k.c.stopProc(k.proc, by, fmt.Sprintf("Container %s definition changed, will be restarted", k.c.spec.Name), false)
	}
}

// restartForSpec says whether the run that ended was killed for a new image:
// the container restarts at once, without back-off and whatever the restart
// policy says.
func (c *ctr) restartForSpec() bool {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if !c.specRestart {
		return false
	}
	c.specRestart = false
	c.restarts++
	return true
}

// applyNewImage prepares the container again after its image changed. false:
// the pod stops.
func (c *ctr) applyNewImage() bool {
	c.w.mu.Lock()
	again := c.reprepare
	// A kill for a new image that came after the run had ended is moot:
	// this run starts with the new image anyway.
	c.reprepare, c.specRestart = false, false
	c.w.mu.Unlock()
	if !again {
		return true
	}
	for {
		err := c.prepare()
		if err == nil {
			return true
		}
		c.setWaiting("CreateContainerConfigError", err.Error())
		c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
		select {
		case <-c.w.ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
}

// hexID is an id like containerd shows in its messages: 64 hex digits.
func hexID() string { return hex.EncodeToString([]byte(randToken(32))) }
