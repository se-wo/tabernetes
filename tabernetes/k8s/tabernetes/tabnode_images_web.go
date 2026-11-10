package main

// nginx and httpd of the built-in images (tabnode_images.go). nginx reads its
// real configuration files: nginx.conf, the includes (conf.d/*.conf),
// server blocks with listen, server_name, root, alias, index, location
// (=, ^~, ~, ~*, prefix, @named), return, try_files, error_page, add_header,
// proxy_pass with upstream blocks and proxy_set_header, autoindex,
// log_format and access_log. So a ConfigMap over /usr/share/nginx/html or
// /etc/nginx/conf.d works like on a real node.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mvdan.cc/sh/v3/interp"
)

const nginxIndexHTML = `<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
`

const nginx50xHTML = `<!DOCTYPE html>
<html>
<head>
<title>Error</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>An error occurred.</h1>
<p>Sorry, the page you are looking for is currently unavailable.<br/>
Please try again later.</p>
<p>If you are the system administrator of this resource then you should check
the error log for details.</p>
<p><em>Faithfully yours, nginx.</em></p>
</body>
</html>
`

const nginxConf = `
user  nginx;
worker_processes  auto;

error_log  /var/log/nginx/error.log notice;
pid        /run/nginx.pid;


events {
    worker_connections  1024;
}


http {
    include       /etc/nginx/mime.types;
    default_type  application/octet-stream;

    log_format  main  '$remote_addr - $remote_user [$time_local] "$request" '
                      '$status $body_bytes_sent "$http_referer" '
                      '"$http_user_agent" "$http_x_forwarded_for"';

    access_log  /var/log/nginx/access.log  main;

    sendfile        on;
    #tcp_nopush     on;

    keepalive_timeout  65;

    #gzip  on;

    include /etc/nginx/conf.d/*.conf;
}
`

const nginxDefaultConf = `server {
    listen       80;
    listen  [::]:80;
    server_name  localhost;

    #access_log  /var/log/nginx/host.access.log  main;

    location / {
        root   /usr/share/nginx/html;
        index  index.html index.htm;
    }

    #error_page  404              /404.html;

    # redirect server error pages to the static page /50x.html
    #
    error_page   500 502 503 504  /50x.html;
    location = /50x.html {
        root   /usr/share/nginx/html;
    }

    # proxy the PHP scripts to Apache listening on 127.0.0.1:80
    #
    #location ~ \.php$ {
    #    proxy_pass   http://127.0.0.1;
    #}

    # deny access to .htaccess files, if Apache's document root
    # concurs with nginx's one
    #
    #location ~ /\.ht {
    #    deny  all;
    #}
}
`

const nginxMimeTypes = `
types {
    text/html                                        html htm shtml;
    text/css                                         css;
    text/xml                                         xml;
    image/gif                                        gif;
    image/jpeg                                       jpeg jpg;
    application/javascript                           js;
    application/atom+xml                             atom;
    application/rss+xml                              rss;

    text/mathml                                      mml;
    text/plain                                       txt;
    text/vnd.sun.j2me.app-descriptor                 jad;
    text/vnd.wap.wml                                 wml;
    text/x-component                                 htc;

    image/avif                                       avif;
    image/png                                        png;
    image/svg+xml                                    svg svgz;
    image/tiff                                       tif tiff;
    image/vnd.wap.wbmp                               wbmp;
    image/webp                                       webp;
    image/x-icon                                     ico;
    image/x-jng                                      jng;
    image/x-ms-bmp                                   bmp;

    font/woff                                        woff;
    font/woff2                                       woff2;

    application/java-archive                         jar war ear;
    application/json                                 json;
    application/mac-binhex40                         hqx;
    application/msword                               doc;
    application/pdf                                  pdf;
    application/postscript                           ps eps ai;
    application/rtf                                  rtf;
    application/vnd.apple.mpegurl                    m3u8;
    application/vnd.ms-excel                         xls;
    application/vnd.ms-fontobject                    eot;
    application/vnd.ms-powerpoint                    ppt;
    application/wasm                                 wasm;
    application/x-7z-compressed                      7z;
    application/x-shockwave-flash                    swf;
    application/xhtml+xml                            xhtml;
    application/zip                                  zip;

    application/octet-stream                         bin exe dll;
    application/octet-stream                         deb;
    application/octet-stream                         dmg;
    application/octet-stream                         iso img;

    audio/midi                                       mid midi kar;
    audio/mpeg                                       mp3;
    audio/ogg                                        ogg;
    audio/x-m4a                                      m4a;

    video/mp2t                                       ts;
    video/mp4                                        mp4;
    video/mpeg                                       mpeg mpg;
    video/quicktime                                  mov;
    video/webm                                       webm;
    video/x-flv                                      flv;
    video/x-m4v                                      m4v;
    video/x-msvideo                                  avi;
}
`

const nginxEntrypointSh = `#!/bin/sh
# Tabernetes: nginx is built into the browser tab. This entrypoint is Go code
# (tabnode_images.go): it runs the templates of /etc/nginx/templates through
# envsubst into /etc/nginx/conf.d, then starts the command.
exec "$@"
`

var nginxFiles = mergeFiles(debianFiles, map[string]string{
	"/usr/share/nginx/html/index.html":                     nginxIndexHTML,
	"/usr/share/nginx/html/50x.html":                       nginx50xHTML,
	"/etc/nginx/nginx.conf":                                nginxConf,
	"/etc/nginx/conf.d/default.conf":                       nginxDefaultConf,
	"/etc/nginx/mime.types":                                nginxMimeTypes,
	"/docker-entrypoint.sh":                                nginxEntrypointSh,
	"/docker-entrypoint.d/10-listen-on-ipv6-by-default.sh": "#!/bin/sh\n# built into tabernetes\n",
	"/docker-entrypoint.d/15-local-resolvers.envsh":        "#!/bin/sh\n# built into tabernetes\n",
	"/docker-entrypoint.d/20-envsubst-on-templates.sh":     "#!/bin/sh\n# built into tabernetes\n",
	"/docker-entrypoint.d/30-tune-worker-processes.sh":     "#!/bin/sh\n# built into tabernetes\n",
	"/var/cache/nginx/.keep":                               "",
})

const httpdIndexHTML = "<html><body><h1>It works!</h1></body></html>\n"

const httpdConf = `#
# This is the main Apache HTTP server configuration file of the httpd image,
# shortened. Tabernetes reads Listen, DocumentRoot, DirectoryIndex and
# ServerTokens of it.
#
ServerRoot "/usr/local/apache2"
Listen 80

LoadModule mpm_event_module modules/mod_mpm_event.so
LoadModule dir_module modules/mod_dir.so
LoadModule autoindex_module modules/mod_autoindex.so
LoadModule mime_module modules/mod_mime.so
LoadModule log_config_module modules/mod_log_config.so

User www-data
Group www-data

ServerAdmin you@example.com

DocumentRoot "/usr/local/apache2/htdocs"
<Directory "/usr/local/apache2/htdocs">
    Options Indexes FollowSymLinks
    AllowOverride None
    Require all granted
</Directory>

<IfModule dir_module>
    DirectoryIndex index.html
</IfModule>

ErrorLog /proc/self/fd/2
LogLevel warn

<IfModule log_config_module>
    LogFormat "%h %l %u %t \"%r\" %>s %b" common
    CustomLog /proc/self/fd/1 common
</IfModule>
`

var httpdFiles = mergeFiles(debianFiles, map[string]string{
	"/usr/local/apache2/htdocs/index.html":    httpdIndexHTML,
	"/usr/local/apache2/conf/httpd.conf":      httpdConf,
	"/usr/local/apache2/bin/httpd-foreground": "#!/bin/sh\nset -e\n\n# Apache gets grumpy about PID files pre-existing\nrm -f /usr/local/apache2/logs/httpd.pid\n\nexec httpd -DFOREGROUND \"$@\"\n",
	"/usr/local/apache2/logs/.keep":           "",
})

func mergeFiles(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// containerFS maps the paths of a program to the file system of the tab:
// through the root of its container.
func containerFS(ctx context.Context) func(string) string {
	if pe := procOf(ctx); pe != nil && pe.root != nil {
		return pe.root.real
	}
	return func(p string) string { return p }
}

// --- nginx: the docker entrypoint ---

const nginxPackagedDefault = nginxDefaultConf

func nginxEntrypoint(ctx context.Context, hc interp.HandlerContext) {
	real := containerFS(ctx)
	out := io.Discard
	if getenv(hc, "NGINX_ENTRYPOINT_QUIET_LOGS") == "" {
		out = hc.Stdout
	}
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	say("/docker-entrypoint.sh: /docker-entrypoint.d/ is not empty, will attempt to perform configuration")
	say("/docker-entrypoint.sh: Looking for shell scripts in /docker-entrypoint.d/")
	say("/docker-entrypoint.sh: Launching /docker-entrypoint.d/10-listen-on-ipv6-by-default.sh")
	const def = "/etc/nginx/conf.d/default.conf"
	if b, err := os.ReadFile(real(def)); err != nil {
		say("10-listen-on-ipv6-by-default.sh: info: %s is not a file or does not exist", def)
	} else if fsAccess(ctx, "/", def, accWrite) != nil { // securityContext, tabnode_security.go
		say("10-listen-on-ipv6-by-default.sh: info: can not modify %s (read-only file system?)", def)
	} else if string(b) != nginxPackagedDefault {
		say("10-listen-on-ipv6-by-default.sh: info: Getting the checksum of %s", def)
		say("10-listen-on-ipv6-by-default.sh: info: %s differs from the packaged version", def)
	} else {
		say("10-listen-on-ipv6-by-default.sh: info: Getting the checksum of %s", def)
		say("10-listen-on-ipv6-by-default.sh: info: Enabled listen on IPv6 in %s", def)
	}
	say("/docker-entrypoint.sh: Sourcing /docker-entrypoint.d/15-local-resolvers.envsh")
	say("/docker-entrypoint.sh: Launching /docker-entrypoint.d/20-envsubst-on-templates.sh")
	envsubstTemplates(ctx, hc, real, say)
	say("/docker-entrypoint.sh: Launching /docker-entrypoint.d/30-tune-worker-processes.sh")
	say("/docker-entrypoint.sh: Configuration complete; ready for start up")
}

// envsubstTemplates is 20-envsubst-on-templates.sh: every
// /etc/nginx/templates/*.template becomes a file in /etc/nginx/conf.d with
// the defined environment variables substituted.
func envsubstTemplates(ctx context.Context, hc interp.HandlerContext, real func(string) string, say func(string, ...any)) {
	dir := getenv(hc, "NGINX_ENVSUBST_TEMPLATE_DIR")
	if dir == "" {
		dir = "/etc/nginx/templates"
	}
	suffix := getenv(hc, "NGINX_ENVSUBST_TEMPLATE_SUFFIX")
	if suffix == "" {
		suffix = ".template"
	}
	outDir := getenv(hc, "NGINX_ENVSUBST_OUTPUT_DIR")
	if outDir == "" {
		outDir = "/etc/nginx/conf.d"
	}
	vars := map[string]string{}
	for _, kv := range environ(hc) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	filepath.WalkDir(real(dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, suffix) || strings.Contains(p, "/..") {
			return nil
		}
		rel, _ := filepath.Rel(real(dir), p)
		tmpl := path.Join(dir, rel)
		target := path.Join(outDir, strings.TrimSuffix(rel, suffix))
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		say("20-envsubst-on-templates.sh: Running envsubst on %s to %s", tmpl, target)
		os.MkdirAll(path.Dir(real(target)), 0o755)
		if err := os.WriteFile(real(target), []byte(envsubst(string(b), vars)), 0o644); err != nil {
			say("20-envsubst-on-templates.sh: ERROR: %s exists, but is not writable", outDir)
		}
		return nil
	})
}

var envVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// envsubst replaces $VAR and ${VAR} of the defined variables only, like
// the nginx image does, so $host and $uri of nginx stay.
func envsubst(s string, vars map[string]string) string {
	return envVarRef.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m, "${}")
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// --- nginx: configuration ---

type ngxDir struct {
	name  string
	args  []string
	block []*ngxDir
	isBlk bool
	file  string
	line  int
}

type ngxErr struct {
	msg  string
	file string
	line int
}

func (e *ngxErr) Error() string {
	if e.file == "" {
		return e.msg
	}
	return fmt.Sprintf("%s in %s:%d", e.msg, e.file, e.line)
}

// ngxParse parses a configuration file and the files it includes.
func ngxParse(real func(string) string, file string, depth int) ([]*ngxDir, error) {
	if depth > 10 {
		return nil, &ngxErr{msg: "too many nested includes"}
	}
	b, err := os.ReadFile(real(file))
	if err != nil {
		return nil, &ngxErr{msg: fmt.Sprintf("open() %q failed (2: No such file or directory)", file)}
	}
	p := &ngxParser{src: string(b), file: file, line: 1}
	dirs, err := p.parse(false)
	if err != nil {
		return nil, err
	}
	return ngxIncludes(real, dirs, depth)
}

func ngxIncludes(real func(string) string, dirs []*ngxDir, depth int) ([]*ngxDir, error) {
	var out []*ngxDir
	for _, d := range dirs {
		if d.name == "include" && len(d.args) == 1 {
			pat := d.args[0]
			if !path.IsAbs(pat) {
				pat = path.Join("/etc/nginx", pat)
			}
			var files []string
			if strings.ContainsAny(pat, "*?[") {
				matches, _ := filepath.Glob(real(path.Dir(pat)) + "/" + path.Base(pat))
				sort.Strings(matches)
				for _, m := range matches {
					files = append(files, path.Join(path.Dir(pat), filepath.Base(m)))
				}
			} else {
				files = []string{pat}
			}
			for _, f := range files {
				sub, err := ngxParse(real, f, depth+1)
				if err != nil {
					var ne *ngxErr
					if errors.As(err, &ne) && ne.file == "" {
						ne.file, ne.line = d.file, d.line
					}
					return nil, err
				}
				out = append(out, sub...)
			}
			continue
		}
		if d.isBlk {
			sub, err := ngxIncludes(real, d.block, depth)
			if err != nil {
				return nil, err
			}
			d.block = sub
		}
		out = append(out, d)
	}
	return out, nil
}

type ngxParser struct {
	src  string
	pos  int
	file string
	line int
}

// token returns the next word, or one of ";", "{", "}" ("" at the end).
func (p *ngxParser) token() (tok string, quoted bool, err error) {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '\n':
			p.line++
			p.pos++
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			goto word
		}
	}
	return "", false, nil
word:
	c := p.src[p.pos]
	if c == ';' || c == '{' || c == '}' {
		p.pos++
		return string(c), false, nil
	}
	if c == '"' || c == '\'' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.src) && p.src[p.pos] != c {
			if p.src[p.pos] == '\\' && p.pos+1 < len(p.src) {
				p.pos++
				switch p.src[p.pos] {
				case c, '\\':
				case 'n':
					b.WriteByte('\n')
					p.pos++
					continue
				default:
					b.WriteByte('\\')
				}
			}
			if p.src[p.pos] == '\n' {
				p.line++
			}
			b.WriteByte(p.src[p.pos])
			p.pos++
		}
		if p.pos >= len(p.src) {
			return "", false, &ngxErr{msg: "unexpected end of file, expecting \";\" or \"}\"", file: p.file, line: p.line}
		}
		p.pos++
		return b.String(), true, nil
	}
	start := p.pos
	depth := 0
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '{' && p.pos > start && p.src[p.pos-1] == '$' {
			depth++ // ${var}
		} else if c == '}' && depth > 0 {
			depth--
		} else if depth == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';' || c == '{' || c == '}') {
			break
		}
		p.pos++
	}
	return p.src[start:p.pos], false, nil
}

func (p *ngxParser) parse(inBlock bool) ([]*ngxDir, error) {
	var out []*ngxDir
	var cur *ngxDir
	for {
		tok, quoted, err := p.token()
		if err != nil {
			return nil, err
		}
		if !quoted {
			switch tok {
			case "":
				if cur != nil || inBlock {
					return nil, &ngxErr{msg: "unexpected end of file, expecting \";\" or \"}\"", file: p.file, line: p.line}
				}
				return out, nil
			case ";":
				if cur == nil {
					return nil, &ngxErr{msg: "unexpected \";\"", file: p.file, line: p.line}
				}
				out = append(out, cur)
				cur = nil
				continue
			case "{":
				if cur == nil {
					return nil, &ngxErr{msg: "unexpected \"{\"", file: p.file, line: p.line}
				}
				sub, err := p.parse(true)
				if err != nil {
					return nil, err
				}
				cur.block, cur.isBlk = sub, true
				out = append(out, cur)
				cur = nil
				continue
			case "}":
				if cur != nil {
					return nil, &ngxErr{msg: "unexpected \"}\"", file: p.file, line: p.line}
				}
				if !inBlock {
					return nil, &ngxErr{msg: "unexpected \"}\"", file: p.file, line: p.line}
				}
				return out, nil
			}
		}
		if cur == nil {
			cur = &ngxDir{name: tok, file: p.file, line: p.line}
		} else {
			cur.args = append(cur.args, tok)
		}
	}
}

// ngxCommon are the directives a location inherits from its server and the
// server from http.
type ngxCommon struct {
	root         string
	alias        string
	index        []string
	errorPages   map[int]ngxErrorPage
	addHeaders   [][2]string
	defaultType  string
	types        map[string]string
	autoindex    bool
	accessLog    []ngxAccessLog
	proxyHeaders [][2]string
	tryFiles     []string
	ret          *ngxReturn
	proxy        string
	serverTokens bool
	internal     bool
	rewrites     []ngxRewrite
}

type ngxErrorPage struct {
	uri  string
	code int // the status of the answer: =code, 0 keeps it
}

type ngxAccessLog struct{ path, format string }

type ngxReturn struct {
	code int
	text string
}

type ngxRewrite struct {
	re   *regexp.Regexp
	repl string
	flag string
}

type ngxLoc struct {
	mod  string // "", "=", "^~", "~", "~*", "@"
	path string
	re   *regexp.Regexp
	c    ngxCommon
}

type ngxServer struct {
	listens  []ngxListen
	names    []string
	c        ngxCommon
	locs     []*ngxLoc
	named    map[string]*ngxLoc
	cert     string
	certKey  string
	defaults map[string]bool // ports it is default_server for
}

type ngxListen struct {
	addr string // host:port for listenPod
	port string
	ssl  bool
	def  bool
}

type ngxConf struct {
	servers    []*ngxServer
	logFormats map[string]string
	upstreams  map[string]*ngxUpstream
	dump       string // nginx -T
}

type ngxUpstream struct {
	servers []string
	next    atomic.Uint32
}

const ngxCombined = `$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"`

// ngxLoad reads the configuration of nginx.
func ngxLoad(real func(string) string, file string, globals string) (*ngxConf, error) {
	dirs, err := ngxParse(real, file, 0)
	if err != nil {
		return nil, err
	}
	if globals != "" {
		p := &ngxParser{src: globals, file: "command line", line: 1}
		g, err := p.parse(false)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, g...)
	}
	cfg := &ngxConf{logFormats: map[string]string{"combined": ngxCombined}, upstreams: map[string]*ngxUpstream{}}
	base := ngxCommon{
		root: "/etc/nginx/html", index: []string{"index.html"}, defaultType: "text/plain", serverTokens: true,
		accessLog: []ngxAccessLog{{"/var/log/nginx/access.log", "combined"}},
	}
	for _, d := range dirs {
		if d.name == "http" && d.isBlk {
			c := base
			if err := cfg.common(&c, d.block, true); err != nil {
				return nil, err
			}
			for _, sd := range d.block {
				switch {
				case sd.name == "server" && sd.isBlk:
					s, err := cfg.server(c, sd)
					if err != nil {
						return nil, err
					}
					cfg.servers = append(cfg.servers, s)
				case sd.name == "upstream" && sd.isBlk && len(sd.args) == 1:
					u := &ngxUpstream{}
					for _, x := range sd.block {
						if x.name == "server" && len(x.args) > 0 {
							u.servers = append(u.servers, x.args[0])
						}
					}
					cfg.upstreams[sd.args[0]] = u
				}
			}
		}
	}
	return cfg, nil
}

func argErr(d *ngxDir) error {
	return &ngxErr{msg: fmt.Sprintf("invalid number of arguments in %q directive", d.name), file: d.file, line: d.line}
}

// common applies the inherited directives of a block to c.
func (cfg *ngxConf) common(c *ngxCommon, dirs []*ngxDir, httpLevel bool) error {
	seen := map[string]bool{}
	for _, d := range dirs {
		first := !seen[d.name]
		seen[d.name] = true
		switch d.name {
		case "root":
			if len(d.args) != 1 {
				return argErr(d)
			}
			c.root, c.alias = d.args[0], ""
		case "alias":
			if len(d.args) != 1 {
				return argErr(d)
			}
			c.alias = d.args[0]
		case "index":
			c.index = append([]string{}, d.args...)
		case "default_type":
			if len(d.args) == 1 {
				c.defaultType = d.args[0]
			}
		case "types":
			if first {
				c.types = map[string]string{}
			}
			for _, t := range d.block {
				for _, ext := range t.args {
					c.types[strings.ToLower(ext)] = t.name
				}
			}
		case "autoindex":
			c.autoindex = len(d.args) == 1 && d.args[0] == "on"
		case "server_tokens":
			c.serverTokens = len(d.args) == 1 && d.args[0] != "off"
		case "error_page":
			if len(d.args) < 2 {
				return argErr(d)
			}
			if first {
				c.errorPages = map[int]ngxErrorPage{}
			}
			ep := ngxErrorPage{uri: d.args[len(d.args)-1]}
			codes := d.args[:len(d.args)-1]
			if last := codes[len(codes)-1]; strings.HasPrefix(last, "=") {
				ep.code, _ = strconv.Atoi(last[1:])
				if ep.code == 0 {
					ep.code = -1 // "=": the status of the answer
				}
				codes = codes[:len(codes)-1]
			}
			for _, s := range codes {
				code, err := strconv.Atoi(s)
				if err != nil {
					return &ngxErr{msg: fmt.Sprintf("invalid value %q", s), file: d.file, line: d.line}
				}
				c.errorPages[code] = ep
			}
		case "add_header":
			if len(d.args) < 2 {
				return argErr(d)
			}
			if first {
				c.addHeaders = nil
			}
			c.addHeaders = append(c.addHeaders, [2]string{d.args[0], d.args[1]})
		case "proxy_set_header":
			if len(d.args) != 2 {
				return argErr(d)
			}
			if first {
				c.proxyHeaders = nil
			}
			c.proxyHeaders = append(c.proxyHeaders, [2]string{d.args[0], d.args[1]})
		case "access_log":
			if len(d.args) == 0 {
				return argErr(d)
			}
			if first {
				c.accessLog = nil
			}
			al := ngxAccessLog{path: d.args[0], format: "combined"}
			if len(d.args) > 1 && !strings.Contains(d.args[1], "=") {
				al.format = d.args[1]
			}
			c.accessLog = append(c.accessLog, al)
		case "log_format":
			if !httpLevel {
				return &ngxErr{msg: "\"log_format\" directive is not allowed here", file: d.file, line: d.line}
			}
			if len(d.args) < 2 {
				return argErr(d)
			}
			args := d.args[1:]
			if strings.HasPrefix(args[0], "escape=") {
				args = args[1:]
			}
			cfg.logFormats[d.args[0]] = strings.Join(args, "")
		}
	}
	return nil
}

// local applies the directives that are not inherited: return, try_files,
// proxy_pass, rewrite, internal.
func (cfg *ngxConf) local(c *ngxCommon, dirs []*ngxDir) error {
	for _, d := range dirs {
		switch d.name {
		case "return":
			if len(d.args) < 1 || len(d.args) > 2 {
				return argErr(d)
			}
			code, err := strconv.Atoi(d.args[0])
			switch {
			case err != nil && len(d.args) == 1:
				c.ret = &ngxReturn{code: 302, text: d.args[0]}
			case err != nil || code < 0 || code > 999:
				return &ngxErr{msg: fmt.Sprintf("invalid return code %q", d.args[0]), file: d.file, line: d.line}
			default:
				c.ret = &ngxReturn{code: code}
				if len(d.args) == 2 {
					c.ret.text = d.args[1]
				}
			}
		case "try_files":
			if len(d.args) < 2 {
				return argErr(d)
			}
			c.tryFiles = d.args
		case "proxy_pass":
			if len(d.args) != 1 {
				return argErr(d)
			}
			if !strings.HasPrefix(d.args[0], "http://") && !strings.HasPrefix(d.args[0], "https://") {
				return &ngxErr{msg: fmt.Sprintf("invalid URL prefix in %q", d.args[0]), file: d.file, line: d.line}
			}
			c.proxy = d.args[0]
		case "rewrite":
			if len(d.args) < 2 {
				return argErr(d)
			}
			re, err := regexp.Compile(d.args[0])
			if err != nil {
				return &ngxErr{msg: fmt.Sprintf("pcre2_compile() failed: %v in %q", err, d.args[0]), file: d.file, line: d.line}
			}
			rw := ngxRewrite{re: re, repl: d.args[1]}
			if len(d.args) > 2 {
				rw.flag = d.args[2]
			}
			c.rewrites = append(c.rewrites, rw)
		case "internal":
			c.internal = true
		}
	}
	return nil
}

func (cfg *ngxConf) server(parent ngxCommon, d *ngxDir) (*ngxServer, error) {
	s := &ngxServer{c: parent, named: map[string]*ngxLoc{}, defaults: map[string]bool{}}
	if err := cfg.common(&s.c, d.block, false); err != nil {
		return nil, err
	}
	if err := cfg.local(&s.c, d.block); err != nil {
		return nil, err
	}
	for _, x := range d.block {
		switch x.name {
		case "listen":
			if len(x.args) == 0 {
				return nil, argErr(x)
			}
			l, ok := ngxParseListen(x.args)
			if !ok {
				continue
			}
			for _, prev := range s.listens {
				if prev.port == l.port {
					ok = false
				}
			}
			if l.def {
				s.defaults[l.port] = true
			}
			if ok {
				s.listens = append(s.listens, l)
			}
		case "server_name":
			s.names = append(s.names, x.args...)
		case "ssl_certificate":
			if len(x.args) == 1 {
				s.cert = x.args[0]
			}
		case "ssl_certificate_key":
			if len(x.args) == 1 {
				s.certKey = x.args[0]
			}
		case "location":
			if err := cfg.location(s, s.c, x); err != nil {
				return nil, err
			}
		}
	}
	if len(s.listens) == 0 {
		s.listens = []ngxListen{{addr: ":80", port: "80"}}
	}
	return s, nil
}

func ngxParseListen(args []string) (ngxListen, bool) {
	a := args[0]
	if strings.HasPrefix(a, "unix:") {
		return ngxListen{}, false
	}
	var l ngxListen
	if _, err := strconv.Atoi(a); err == nil {
		l.port, l.addr = a, ":"+a
	} else if host, port, err := net.SplitHostPort(a); err == nil {
		if host == "*" || host == "::" {
			host = ""
		}
		l.port, l.addr = port, net.JoinHostPort(host, port)
	} else {
		l.port, l.addr = "80", net.JoinHostPort(strings.Trim(a, "[]"), "80")
	}
	for _, f := range args[1:] {
		switch f {
		case "ssl":
			l.ssl = true
		case "default_server", "default":
			l.def = true
		}
	}
	return l, true
}

func (cfg *ngxConf) location(s *ngxServer, parent ngxCommon, d *ngxDir) error {
	if len(d.args) < 1 || len(d.args) > 2 {
		return argErr(d)
	}
	l := &ngxLoc{c: parent}
	// not inherited
	l.c.ret, l.c.tryFiles, l.c.proxy, l.c.rewrites, l.c.internal = nil, nil, "", nil, false
	if len(d.args) == 2 {
		l.mod, l.path = d.args[0], d.args[1]
	} else if strings.HasPrefix(d.args[0], "@") {
		l.mod, l.path = "@", d.args[0]
	} else if strings.HasPrefix(d.args[0], "=") && len(d.args[0]) > 1 {
		l.mod, l.path = "=", d.args[0][1:]
	} else {
		l.path = d.args[0]
	}
	switch l.mod {
	case "", "=", "^~", "@":
	case "~", "~*":
		expr := l.path
		if l.mod == "~*" {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return &ngxErr{msg: fmt.Sprintf("pcre2_compile() failed: %v in %q", err, l.path), file: d.file, line: d.line}
		}
		l.re = re
	default:
		return &ngxErr{msg: fmt.Sprintf("invalid location modifier %q", l.mod), file: d.file, line: d.line}
	}
	if err := cfg.common(&l.c, d.block, false); err != nil {
		return err
	}
	if err := cfg.local(&l.c, d.block); err != nil {
		return err
	}
	if l.mod == "@" {
		s.named[l.path] = l
	} else {
		s.locs = append(s.locs, l)
	}
	// Nested locations are matched like the others of the server.
	for _, x := range d.block {
		if x.name == "location" {
			if err := cfg.location(s, l.c, x); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ngxServer) match(uri string) *ngxLoc {
	var best *ngxLoc
	for _, l := range s.locs {
		if l.mod == "=" && uri == l.path {
			return l
		}
	}
	for _, l := range s.locs {
		if (l.mod == "" || l.mod == "^~") && strings.HasPrefix(uri, l.path) && (best == nil || len(l.path) > len(best.path)) {
			best = l
		}
	}
	if best != nil && best.mod == "^~" {
		return best
	}
	for _, l := range s.locs {
		if l.re != nil && l.re.MatchString(uri) {
			return l
		}
	}
	return best
}

// pick chooses the server of a request: by server_name among the servers
// listening on the port, else the default server of the port.
func (cfg *ngxConf) pick(port, host string) *ngxServer {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	var first, def *ngxServer
	for _, s := range cfg.servers {
		on := false
		for _, l := range s.listens {
			if l.port == port {
				on = true
			}
		}
		if !on {
			continue
		}
		if first == nil {
			first = s
		}
		if s.defaults[port] && def == nil {
			def = s
		}
		for _, n := range s.names {
			n = strings.ToLower(n)
			switch {
			case n == host:
				return s
			case strings.HasPrefix(n, "*.") && strings.HasSuffix(host, n[1:]):
				return s
			case strings.HasPrefix(n, ".") && (host == n[1:] || strings.HasSuffix(host, n)):
				return s
			case strings.HasPrefix(n, "~"):
				if re, err := regexp.Compile(n[1:]); err == nil && re.MatchString(host) {
					return s
				}
			}
		}
	}
	if def != nil {
		return def
	}
	return first
}

// --- nginx: the program ---

type nginxInst struct {
	cfg     atomic.Pointer[ngxConf]
	ctx     context.Context
	hc      interp.HandlerContext
	stop    context.CancelFunc
	real    func(string) string
	file    string
	globals string
	version string
	ports   map[string]bool
	mu      sync.Mutex
	errw    *syncWriter
	outw    *syncWriter
	conns   atomic.Uint64
}

// nginxRunning are the nginx masters by container, for nginx -s.
var nginxRunning sync.Map // *ctr -> *nginxInst

func nginxVersion(hc interp.HandlerContext) string {
	if v := getenv(hc, "NGINX_VERSION"); v != "" {
		return v
	}
	return "1.29.3"
}

func nginxProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	version := nginxVersion(hc)
	file := "/etc/nginx/nginx.conf"
	var globals, signal string
	var test, dump, quiet bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, bool) {
			if len(a) > 2 {
				return a[2:], true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			fmt.Fprintf(hc.Stderr, "nginx: option \"%s\" requires %s\n", a[:2], map[byte]string{'c': "file name", 'g': "directives", 's': "signal", 'p': "path", 'e': "file name"}[a[1]])
			return "", false
		}
		if len(a) < 2 || a[0] != '-' {
			fmt.Fprintf(hc.Stderr, "nginx: invalid option: \"%s\"\n", a)
			return interp.ExitStatus(1)
		}
		switch a[1] {
		case 'v':
			fmt.Fprintf(hc.Stderr, "nginx version: nginx/%s\n", version)
			return nil
		case 'V':
			fmt.Fprintf(hc.Stderr, "nginx version: nginx/%s\nbuilt into Tabernetes (Go, js/wasm): a reimplementation of the parts of nginx that tutorials use\nconfigure arguments: --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx --conf-path=/etc/nginx/nginx.conf --http-log-path=/var/log/nginx/access.log --error-log-path=/var/log/nginx/error.log\n", version)
			return nil
		case 'h', '?':
			fmt.Fprintf(hc.Stderr, "nginx version: nginx/%s\nUsage: nginx [-?hvVtTq] [-s signal] [-p prefix]\n             [-e filename] [-c filename] [-g directives]\n", version)
			return nil
		case 't':
			test = true
		case 'T':
			test, dump = true, true
		case 'q':
			quiet = true
		case 'c':
			v, ok := val()
			if !ok {
				return interp.ExitStatus(1)
			}
			file = v
			if !path.IsAbs(file) {
				file = path.Join("/etc/nginx", file)
			}
		case 'g':
			v, ok := val()
			if !ok {
				return interp.ExitStatus(1)
			}
			globals = v
		case 's':
			v, ok := val()
			if !ok {
				return interp.ExitStatus(1)
			}
			signal = v
		case 'p', 'e':
			if _, ok := val(); !ok {
				return interp.ExitStatus(1)
			}
		default:
			fmt.Fprintf(hc.Stderr, "nginx: invalid option: \"%s\"\n", a[1:])
			return interp.ExitStatus(1)
		}
	}
	real := containerFS(ctx)
	errlog := func(level, format string, a ...any) {
		fmt.Fprintf(hc.Stderr, "%s [%s] 1#1: %s\n", time.Now().UTC().Format("2006/01/02 15:04:05"), level, fmt.Sprintf(format, a...))
	}
	cfg, err := ngxLoad(real, file, globals)
	if signal != "" {
		return nginxSignal(ctx, hc, signal)
	}
	if test {
		if err != nil {
			fmt.Fprintf(hc.Stderr, "nginx: [emerg] %v\nnginx: configuration file %s test failed\n", err, file)
			return interp.ExitStatus(1)
		}
		if !quiet {
			fmt.Fprintf(hc.Stderr, "nginx: the configuration file %s syntax is ok\nnginx: configuration file %s test is successful\n", file, file)
		}
		if dump {
			b, _ := os.ReadFile(real(file))
			fmt.Fprintf(hc.Stdout, "# configuration file %s:\n%s\n", file, b)
		}
		return nil
	}
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s [emerg] 1#1: %v\nnginx: [emerg] %v\n", time.Now().UTC().Format("2006/01/02 15:04:05"), err, err)
		return interp.ExitStatus(1)
	}
	if err := nginxStartChecks(ctx, hc, real, file); err != nil { // tabnode_security.go
		return err
	}
	// daemon on (without -g "daemon off;") would fork into the background;
	// here nginx always stays in the foreground.

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inst := &nginxInst{ctx: ctx, hc: hc, stop: cancel, real: real, file: file, globals: globals, version: version,
		ports: map[string]bool{}, errw: &syncWriter{w: hc.Stderr}, outw: &syncWriter{w: hc.Stdout}}
	inst.cfg.Store(cfg)
	if err := inst.listen(cfg); err != nil {
		return interp.ExitStatus(1)
	}
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		nginxRunning.Store(pe.c, inst)
		defer nginxRunning.CompareAndDelete(pe.c, inst)
	}
	errlog("notice", "using the \"epoll\" event method")
	errlog("notice", "nginx/%s", version)
	errlog("notice", "built into Tabernetes (Go %s, js/wasm)", strings.TrimPrefix(runtime.Version(), "go"))
	errlog("notice", "OS: Linux 6.0.0-tabernetes")
	errlog("notice", "getrlimit(RLIMIT_NOFILE): 1048576:1048576")
	errlog("notice", "start worker processes")
	errlog("notice", "start worker process 29")
	<-ctx.Done()
	return nil
}

// nginxSignal is nginx -s: it signals the master of the container.
func nginxSignal(ctx context.Context, hc interp.HandlerContext, signal string) error {
	var inst *nginxInst
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		if v, ok := nginxRunning.Load(pe.c); ok {
			inst = v.(*nginxInst)
		}
	}
	switch signal {
	case "stop", "quit", "reload", "reopen":
	default:
		fmt.Fprintf(hc.Stderr, "nginx: invalid option: \"-s %s\"\n", signal)
		return interp.ExitStatus(1)
	}
	if inst == nil {
		fmt.Fprintf(hc.Stderr, "nginx: [error] open() \"/run/nginx.pid\" failed (2: No such file or directory)\n")
		return interp.ExitStatus(1)
	}
	fmt.Fprintf(hc.Stderr, "%s [notice] 42#42: signal process started\n", time.Now().UTC().Format("2006/01/02 15:04:05"))
	inst.signal(signal)
	return nil
}

func (n *nginxInst) errlog(level, format string, a ...any) {
	fmt.Fprintf(n.errw, "%s [%s] 29#29: %s\n", time.Now().UTC().Format("2006/01/02 15:04:05"), level, fmt.Sprintf(format, a...))
}

func (n *nginxInst) signal(sig string) {
	master := func(format string, a ...any) {
		fmt.Fprintf(n.errw, "%s [notice] 1#1: %s\n", time.Now().UTC().Format("2006/01/02 15:04:05"), fmt.Sprintf(format, a...))
	}
	switch sig {
	case "stop":
		master("signal 15 (SIGTERM) received from 42, exiting")
		master("exit")
		n.stop()
	case "quit":
		master("signal 3 (SIGQUIT) received from 42, shutting down")
		master("exit")
		n.stop()
	case "reopen":
		master("signal 10 (SIGUSR1) received from 42, reopening logs")
	case "reload":
		master("signal 1 (SIGHUP) received from 42, reconfiguring")
		master("reconfiguring")
		cfg, err := ngxLoad(n.real, n.file, n.globals)
		if err != nil {
			fmt.Fprintf(n.errw, "%s [emerg] 1#1: %v\n", time.Now().UTC().Format("2006/01/02 15:04:05"), err)
			return
		}
		if err := n.listen(cfg); err != nil {
			return
		}
		n.cfg.Store(cfg)
		master("start worker processes")
		master("start worker process 43")
		master("gracefully shutting down")
	}
}

// listen opens the ports of cfg that are not open yet.
func (n *nginxInst) listen(cfg *ngxConf) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	type lis struct {
		l   ngxListen
		srv *ngxServer
	}
	var todo []lis
	seen := map[string]bool{}
	for _, s := range cfg.servers {
		for _, l := range s.listens {
			if n.ports[l.port] || seen[l.port] {
				continue
			}
			seen[l.port] = true
			todo = append(todo, lis{l, s})
		}
	}
	var opened []net.Listener
	for _, t := range todo {
		ln, err := listenPod(n.ctx, t.l.addr)
		if err != nil {
			n.errlog("emerg", "bind() to %s failed (98: Address already in use)", t.l.addr)
			fmt.Fprintf(n.errw, "nginx: [emerg] bind() to %s failed (98: Address already in use)\n", t.l.addr)
			for _, o := range opened {
				o.Close()
			}
			return err
		}
		if t.l.ssl {
			cert, err := tls.LoadX509KeyPair(n.real(t.srv.cert), n.real(t.srv.certKey))
			if err != nil {
				ln.Close()
				fmt.Fprintf(n.errw, "nginx: [emerg] cannot load certificate %q: %v\n", t.srv.cert, err)
				return err
			}
			ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
		}
		opened = append(opened, ln)
		n.ports[t.l.port] = true
		port, ssl := t.l.port, t.l.ssl
		go serveListeners(n.ctx, n.hc, "nginx", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.serve(w, r, port, ssl)
		}), ln)
	}
	return nil
}

// ngxReq is one request in nginx.
type ngxReq struct {
	n      *nginxInst
	cfg    *ngxConf
	srv    *ngxServer
	w      *statusWriter
	r      *http.Request
	port   string
	ssl    bool
	uri    string // $uri: the normalized path, changed by internal redirects
	args   string
	start  time.Time
	conn   uint64
	status int // the status of an error_page answer
	depth  int
	loc    *ngxLoc
	c      *ngxCommon
	caps   []string // captures of the regex location or rewrite
	reqID  string   // $request_id, made on first use
	file   string   // $request_filename
}

func (n *nginxInst) serve(w http.ResponseWriter, r *http.Request, port string, ssl bool) {
	cfg := n.cfg.Load()
	q := &ngxReq{n: n, cfg: cfg, w: &statusWriter{ResponseWriter: w}, r: r, port: port, ssl: ssl,
		args: r.URL.RawQuery, start: time.Now(), conn: n.conns.Add(1)}
	q.srv = cfg.pick(port, r.Host)
	q.uri = ngxNormalize(r.URL.Path)
	if q.srv == nil {
		q.respondError(http.StatusNotFound)
	} else {
		q.handle()
	}
	q.accessLog()
}

func ngxNormalize(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func (q *ngxReq) common() *ngxCommon {
	if q.c != nil {
		return q.c
	}
	return &q.srv.c
}

func (q *ngxReq) handle() {
	if q.depth > 10 {
		q.n.errlog("error", "*%d rewrite or internal redirection cycle while processing %q, client: %s", q.conn, q.uri, remoteHost(q.r))
		q.respondError(http.StatusInternalServerError)
		return
	}
	q.depth++
	if q.depth == 1 {
		// the server level runs once: rewrite, return
		if q.rewrite(q.srv.c.rewrites) {
			return
		}
		if ret := q.srv.c.ret; ret != nil {
			q.doReturn(ret)
			return
		}
	}
	q.loc = q.srv.match(q.uri)
	q.c, q.caps = &q.srv.c, nil
	if q.loc != nil {
		q.c = &q.loc.c
		if q.loc.re != nil {
			q.caps = q.loc.re.FindStringSubmatch(q.uri)
		}
		if q.loc.c.internal && q.depth == 1 {
			q.respondError(http.StatusNotFound)
			return
		}
		if q.rewrite(q.loc.c.rewrites) {
			return
		}
	}
	q.run()
}

// run handles the request in its location (or a named location).
func (q *ngxReq) run() {
	c := q.c
	switch {
	case c.ret != nil:
		q.doReturn(c.ret)
	case c.proxy != "":
		q.proxy(c.proxy)
	case len(c.tryFiles) > 0:
		q.tryFiles(c.tryFiles)
	default:
		q.static(q.uri)
	}
}

// rewrite applies rewrite directives; it reports whether the request was
// answered or redirected.
func (q *ngxReq) rewrite(rules []ngxRewrite) bool {
	for _, rw := range rules {
		m := rw.re.FindStringSubmatch(q.uri)
		if m == nil {
			continue
		}
		q.caps = m
		target := q.expand(rw.repl)
		if rw.flag == "redirect" || rw.flag == "permanent" || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			code := http.StatusFound
			if rw.flag == "permanent" {
				code = http.StatusMovedPermanently
			}
			if q.args != "" && !strings.HasSuffix(rw.repl, "?") {
				if strings.Contains(target, "?") {
					target += "&" + q.args
				} else {
					target += "?" + q.args
				}
			}
			q.redirect(code, strings.TrimSuffix(target, "?"))
			return true
		}
		if u, a, ok := strings.Cut(target, "?"); ok {
			target = u
			if a != "" {
				q.args = a
			}
		}
		q.uri = target
		switch rw.flag {
		case "last":
			q.handle()
			return true
		case "break":
			return false
		}
	}
	return false
}

func (q *ngxReq) setHeaders(code int) {
	h := q.w.Header()
	if q.common().serverTokens {
		h.Set("Server", "nginx/"+q.n.version)
	} else {
		h.Set("Server", "nginx")
	}
	switch code {
	case 200, 201, 204, 206, 301, 302, 303, 304, 307, 308:
		for _, kv := range q.common().addHeaders {
			if v := q.expand(kv[1]); v != "" {
				h.Add(kv[0], v)
			}
		}
	}
}

func (q *ngxReq) doReturn(ret *ngxReturn) {
	code := ret.code
	text := q.expand(ret.text)
	switch {
	case code == 444:
		if hj, ok := q.w.ResponseWriter.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				q.w.status = 444
				return
			}
		}
		q.respondError(http.StatusBadRequest)
	case code == 301 || code == 302 || code == 303 || code == 307 || code == 308:
		q.redirect(code, text)
	case text == "" && code >= 400:
		q.respondError(code)
	default:
		if code == 0 {
			code = http.StatusOK
		}
		ct := q.common().defaultType
		q.setHeaders(code)
		q.w.Header().Set("Content-Type", ct)
		q.w.Header().Set("Content-Length", strconv.Itoa(len(text)))
		q.w.WriteHeader(code)
		if q.r.Method != http.MethodHead {
			io.WriteString(q.w, text)
		}
	}
}

func (q *ngxReq) redirect(code int, target string) {
	if strings.HasPrefix(target, "/") {
		target = q.scheme() + "://" + q.hostForRedirect() + target
	}
	q.w.Header().Set("Location", target)
	q.respondError(code)
}

func (q *ngxReq) scheme() string {
	if q.ssl {
		return "https"
	}
	return "http"
}

func (q *ngxReq) hostForRedirect() string {
	host := q.r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = podIP(q.r.Context())
	}
	if (q.port == "80" && !q.ssl) || (q.port == "443" && q.ssl) {
		return host
	}
	return net.JoinHostPort(host, q.port)
}

var ngxStatusText = map[int]string{
	301: "Moved Permanently", 302: "Found", 303: "See Other", 307: "Temporary Redirect", 308: "Permanent Redirect",
	400: "Bad Request", 401: "Authorization Required", 403: "Forbidden", 404: "Not Found", 405: "Not Allowed",
	408: "Request Time-out", 413: "Request Entity Too Large", 414: "Request-URI Too Large", 429: "Too Many Requests",
	500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway", 503: "Service Temporarily Unavailable",
	504: "Gateway Time-out",
}

// respondError answers with code: through error_page when one is set, else
// with the built-in page of nginx.
func (q *ngxReq) respondError(code int) {
	c := q.common()
	if ep, ok := c.errorPages[code]; ok && q.status == 0 && q.depth < 10 {
		status := code
		if ep.code > 0 {
			status = ep.code
		}
		if strings.HasPrefix(ep.uri, "http://") || strings.HasPrefix(ep.uri, "https://") {
			q.status = -1
			q.redirect(http.StatusFound, ep.uri)
			return
		}
		q.status = status
		if strings.HasPrefix(ep.uri, "@") {
			if l := q.srv.named[ep.uri]; l != nil {
				q.loc, q.c = l, &l.c
				q.run()
				return
			}
		}
		u, a, _ := strings.Cut(ep.uri, "?")
		q.uri, q.args = u, a
		if q.r.Method != http.MethodHead {
			q.r = q.r.Clone(q.r.Context())
			q.r.Method = http.MethodGet
		}
		q.handle()
		return
	}
	text := ngxStatusText[code]
	if text == "" {
		text = http.StatusText(code)
	}
	var body string
	if code >= 300 {
		sig := "nginx"
		if c.serverTokens {
			sig += "/" + q.n.version
		}
		body = fmt.Sprintf("<html>\r\n<head><title>%d %s</title></head>\r\n<body>\r\n<center><h1>%d %s</h1></center>\r\n<hr><center>%s</center>\r\n</body>\r\n</html>\r\n", code, text, code, text, sig)
	}
	q.setHeaders(code)
	if body != "" {
		q.w.Header().Set("Content-Type", "text/html")
		q.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	q.w.WriteHeader(code)
	if q.r.Method != http.MethodHead {
		io.WriteString(q.w, body)
	}
}

// path of the file for uri: root or alias of the location.
func (q *ngxReq) filePath(uri string) string {
	c := q.common()
	if c.alias != "" && q.loc != nil {
		alias := q.expand(c.alias)
		if q.loc.re != nil {
			return alias
		}
		return alias + strings.TrimPrefix(uri, q.loc.path)
	}
	root := q.expand(c.root)
	if !path.IsAbs(root) {
		root = path.Join("/etc/nginx", root)
	}
	return strings.TrimSuffix(root, "/") + uri
}

func (q *ngxReq) tryFiles(list []string) {
	for _, f := range list[:len(list)-1] {
		uri := q.expand(f)
		p := q.filePath(uri)
		fi, err := os.Stat(q.n.real(p))
		if err != nil {
			continue
		}
		if strings.HasSuffix(uri, "/") {
			if fi.IsDir() {
				q.static(uri)
				return
			}
			continue
		}
		if fi.Mode().IsRegular() {
			q.uri = uri
			q.static(uri)
			return
		}
	}
	last := list[len(list)-1]
	switch {
	case strings.HasPrefix(last, "="):
		code, _ := strconv.Atoi(last[1:])
		q.respondError(code)
	case strings.HasPrefix(last, "@"):
		if l := q.srv.named[last]; l != nil {
			q.loc, q.c = l, &l.c
			q.run()
			return
		}
		q.respondError(http.StatusInternalServerError)
	default:
		u, a, ok := strings.Cut(q.expand(last), "?")
		q.uri = u
		if ok {
			q.args = a
		}
		q.handle()
	}
}

// static serves a file or directory of the root.
func (q *ngxReq) static(uri string) {
	if q.r.Method != http.MethodGet && q.r.Method != http.MethodHead {
		q.respondError(http.StatusMethodNotAllowed)
		return
	}
	c := q.common()
	file := q.filePath(uri)
	q.file = file
	real := q.n.real(file)
	fi, err := os.Stat(real)
	if err != nil {
		q.n.errlog("error", "*%d open() %q failed (2: No such file or directory), client: %s, server: %s, request: %q, host: %q",
			q.conn, file, remoteHost(q.r), q.serverName(), q.requestLine(), q.r.Host)
		q.respondError(http.StatusNotFound)
		return
	}
	if fi.IsDir() {
		if !strings.HasSuffix(uri, "/") {
			q.redirect(http.StatusMovedPermanently, (&url.URL{Path: uri + "/"}).EscapedPath())
			return
		}
		for _, idx := range c.index {
			idx = q.expand(idx)
			if strings.HasPrefix(idx, "/") {
				q.uri = idx
				q.handle()
				return
			}
			if ifi, err := os.Stat(q.n.real(file + idx)); err == nil && ifi.Mode().IsRegular() {
				// an internal redirect to the index file
				q.uri = uri + idx
				q.handle()
				return
			}
		}
		if c.autoindex {
			q.autoindex(uri, real)
			return
		}
		q.n.errlog("error", "*%d directory index of %q is forbidden, client: %s, server: %s, request: %q, host: %q",
			q.conn, file, remoteHost(q.r), q.serverName(), q.requestLine(), q.r.Host)
		q.respondError(http.StatusForbidden)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		q.respondError(http.StatusForbidden)
		return
	}
	defer f.Close()
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(file), "."))
	ct := c.defaultType
	if t, ok := c.types[ext]; ok {
		ct = t
	} else if c.types == nil {
		if t := mime.TypeByExtension("." + ext); t != "" {
			ct = t
		}
	}
	code := http.StatusOK
	if q.status > 0 {
		code = q.status
	}
	q.setHeaders(code)
	h := q.w.Header()
	h.Set("Content-Type", ct)
	if code != http.StatusOK {
		h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		q.w.WriteHeader(code)
		if q.r.Method != http.MethodHead {
			io.Copy(q.w, f)
		}
		return
	}
	h.Set("ETag", fmt.Sprintf(`"%x-%x"`, fi.ModTime().Unix(), fi.Size()))
	h.Set("Accept-Ranges", "bytes")
	http.ServeContent(q.w, q.r, "", fi.ModTime(), f)
}

func (q *ngxReq) autoindex(uri, real string) {
	entries, err := os.ReadDir(real)
	if err != nil {
		q.respondError(http.StatusForbidden)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<html>\r\n<head><title>Index of %s</title></head>\r\n<body>\r\n<h1>Index of %s</h1><hr><pre><a href=\"../\">../</a>\r\n", html.EscapeString(uri), html.EscapeString(uri))
	var dirs, files []string
	infos := map[string]fs.FileInfo{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := os.Stat(filepath.Join(real, e.Name()))
		if err != nil {
			continue
		}
		infos[e.Name()] = fi
		if fi.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	for _, name := range append(dirs, files...) {
		fi := infos[name]
		disp, size := name, strconv.FormatInt(fi.Size(), 10)
		if fi.IsDir() {
			disp, size = name+"/", "-"
		}
		shown := disp
		if len(shown) > 50 {
			shown = shown[:47] + "..>"
		}
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a>%s %s %19s\r\n", url.PathEscape(name)+strings.TrimPrefix(disp, name), html.EscapeString(shown),
			strings.Repeat(" ", max(51-len(shown)-1, 0)), fi.ModTime().UTC().Format("02-Jan-2006 15:04"), size)
	}
	b.WriteString("</pre><hr></body>\r\n</html>\r\n")
	q.setHeaders(http.StatusOK)
	q.w.Header().Set("Content-Type", "text/html")
	q.w.WriteHeader(http.StatusOK)
	if q.r.Method != http.MethodHead {
		io.WriteString(q.w, b.String())
	}
}

// ngxClient sends proxied requests: into the cluster through the network of
// the pod, else with fetch(). Redirects go back to the client.
var ngxClient = &http.Client{
	Transport: &http.Transport{DialContext: dialCluster, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var ngxFetch = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (q *ngxReq) proxy(target string) {
	target = q.expand(target)
	u, err := url.Parse(target)
	if err != nil {
		q.respondError(http.StatusInternalServerError)
		return
	}
	proxyHost := u.Host
	upstream := u.Host
	if up := q.cfg.upstreams[u.Host]; up != nil && len(up.servers) > 0 {
		upstream = up.servers[int(up.next.Add(1)-1)%len(up.servers)]
	}
	if _, _, err := net.SplitHostPort(upstream); err != nil {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		upstream = net.JoinHostPort(upstream, port)
	}
	// With a URI in proxy_pass, the matched part of the location is
	// replaced by it; without, the request URI goes as it is.
	reqURI := q.r.URL.RequestURI()
	if u.Path != "" || strings.HasSuffix(target, "/") {
		rest := q.uri
		if q.loc != nil && q.loc.re == nil && q.loc.mod != "@" {
			rest = strings.TrimPrefix(q.uri, q.loc.path)
		}
		reqURI = u.Path + rest
		if q.args != "" {
			reqURI += "?" + q.args
		}
	}
	out, err := http.NewRequestWithContext(q.r.Context(), q.r.Method, u.Scheme+"://"+upstream+reqURI, q.r.Body)
	if err != nil {
		q.respondError(http.StatusInternalServerError)
		return
	}
	out.ContentLength = q.r.ContentLength
	for k, v := range q.r.Header {
		switch k {
		case "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Proxy-Connection":
			continue
		}
		out.Header[k] = v
	}
	out.Host = proxyHost
	for _, kv := range q.common().proxyHeaders {
		v := q.expand(kv[1])
		if strings.EqualFold(kv[0], "Host") {
			if v != "" {
				out.Host = v
			}
			continue
		}
		if v == "" {
			out.Header.Del(kv[0])
		} else {
			out.Header.Set(kv[0], v)
		}
	}
	client := ngxFetch
	host, _, _ := net.SplitHostPort(upstream)
	if inCluster(q.r.Context(), host) {
		client = ngxClient
	}
	resp, err := client.Do(out)
	if err != nil {
		what := "connect() failed (111: Connection refused) while connecting to upstream"
		code := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			what, code = "upstream timed out (110: Connection timed out) while connecting to upstream", http.StatusGatewayTimeout
		} else if strings.Contains(err.Error(), "no such host") {
			what = fmt.Sprintf("%s could not be resolved (3: Host not found)", host)
		}
		q.n.errlog("error", "*%d %s, client: %s, server: %s, request: %q, upstream: %q, host: %q",
			q.conn, what, remoteHost(q.r), q.serverName(), q.requestLine(), u.Scheme+"://"+upstream+reqURI, q.r.Host)
		q.respondError(code)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		switch k {
		case "Server", "Date", "Connection", "Keep-Alive", "Transfer-Encoding", "X-Pad":
			continue
		}
		q.w.Header()[k] = v
	}
	q.setHeaders(resp.StatusCode)
	q.w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := q.w.Write(buf[:n]); werr != nil {
				return
			}
			q.w.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (q *ngxReq) serverName() string {
	if len(q.srv.names) > 0 {
		return q.srv.names[0]
	}
	return ""
}

func (q *ngxReq) requestLine() string {
	return fmt.Sprintf("%s %s %s", q.r.Method, q.r.URL.RequestURI(), q.r.Proto)
}

var ngxVarRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}|\$([A-Za-z0-9_]+)`)

// expand substitutes the variables of nginx in s.
func (q *ngxReq) expand(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return ngxVarRef.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := q.variable(strings.Trim(m, "${}"))
		return v
	})
}

func (q *ngxReq) variable(name string) (string, bool) {
	r := q.r
	if n, err := strconv.Atoi(name); err == nil {
		if n < len(q.caps) {
			return q.caps[n], true
		}
		return "", false
	}
	switch name {
	case "remote_addr", "realip_remote_addr", "binary_remote_addr":
		return remoteHost(r), true
	case "remote_port":
		_, p, _ := net.SplitHostPort(r.RemoteAddr)
		return p, true
	case "remote_user":
		if u, _, ok := r.BasicAuth(); ok {
			return u, true
		}
		return "", false
	case "time_local":
		return time.Now().UTC().Format("02/Jan/2006:15:04:05 -0700"), true
	case "time_iso8601":
		return time.Now().UTC().Format("2006-01-02T15:04:05-07:00"), true
	case "msec":
		return fmt.Sprintf("%.3f", float64(time.Now().UnixMilli())/1000), true
	case "request":
		return q.requestLine(), true
	case "request_method":
		return r.Method, true
	case "request_uri":
		return r.URL.RequestURI(), true
	case "uri", "document_uri":
		return q.uri, true
	case "args", "query_string":
		return q.args, q.args != ""
	case "is_args":
		if q.args != "" {
			return "?", true
		}
		return "", true
	case "scheme":
		return q.scheme(), true
	case "https":
		if q.ssl {
			return "on", true
		}
		return "", true
	case "server_protocol":
		return r.Proto, true
	case "status":
		return strconv.Itoa(q.w.code()), true
	case "body_bytes_sent":
		return strconv.Itoa(q.w.size), true
	case "bytes_sent":
		return strconv.Itoa(q.w.size + 200), true
	case "request_time":
		return fmt.Sprintf("%.3f", time.Since(q.start).Seconds()), true
	case "request_length":
		return strconv.FormatInt(max(r.ContentLength, 0)+int64(len(q.requestLine())), 10), true
	case "host":
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		if h == "" {
			h = q.serverName()
		}
		return strings.ToLower(h), true
	case "hostname":
		return podHostname(r.Context()), true
	case "request_id":
		if q.reqID == "" {
			b := make([]byte, 16)
			rand.Read(b)
			q.reqID = hex.EncodeToString(b)
		}
		return q.reqID, true
	case "server_name":
		return q.serverName(), true
	case "server_port":
		return q.port, true
	case "server_addr":
		return podIP(r.Context()), true
	case "document_root":
		return q.expand(q.common().root), true
	case "request_filename":
		return q.filePath(q.uri), true
	case "content_type":
		return r.Header.Get("Content-Type"), r.Header.Get("Content-Type") != ""
	case "content_length":
		return r.Header.Get("Content-Length"), r.Header.Get("Content-Length") != ""
	case "connection":
		return strconv.FormatUint(q.conn, 10), true
	case "pid":
		return "29", true
	case "nginx_version":
		return q.n.version, true
	case "proxy_host":
		return "", false
	case "proxy_add_x_forwarded_for":
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return xff + ", " + remoteHost(r), true
		}
		return remoteHost(r), true
	}
	if h, ok := strings.CutPrefix(name, "http_"); ok {
		v := r.Header.Get(strings.ReplaceAll(h, "_", "-"))
		return v, v != ""
	}
	if h, ok := strings.CutPrefix(name, "sent_http_"); ok {
		v := q.w.Header().Get(strings.ReplaceAll(h, "_", "-"))
		return v, v != ""
	}
	if a, ok := strings.CutPrefix(name, "arg_"); ok {
		vals, _ := url.ParseQuery(q.args)
		for k, v := range vals {
			if strings.EqualFold(k, a) && len(v) > 0 {
				return v[0], true
			}
		}
		return "", false
	}
	if c, ok := strings.CutPrefix(name, "cookie_"); ok {
		if ck, err := r.Cookie(c); err == nil {
			return ck.Value, true
		}
		return "", false
	}
	return "", false
}

// accessLog writes the access log lines of a request.
func (q *ngxReq) accessLog() {
	c := q.common()
	for _, al := range c.accessLog {
		var w io.Writer
		switch al.path {
		case "off":
			continue
		case "/var/log/nginx/access.log", "/dev/stdout", "/proc/self/fd/1":
			w = q.n.outw
		case "/var/log/nginx/error.log", "/dev/stderr", "/proc/self/fd/2":
			w = q.n.errw
		default:
			if strings.HasPrefix(al.path, "syslog:") {
				continue
			}
			f, err := os.OpenFile(q.n.real(q.expand(al.path)), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
			if err != nil {
				continue
			}
			defer f.Close()
			w = f
		}
		format, ok := q.cfg.logFormats[al.format]
		if !ok {
			format = ngxCombined
		}
		line := ngxVarRef.ReplaceAllStringFunc(format, func(m string) string {
			v, ok := q.variable(strings.Trim(m, "${}"))
			if !ok || v == "" {
				return "-"
			}
			return strings.ReplaceAll(v, `"`, `\x22`)
		})
		io.WriteString(w, line+"\n")
	}
}

// --- httpd ---

// httpdProg is httpd of the httpd image: httpd-foreground, httpd and
// apachectl. It reads Listen, DocumentRoot, DirectoryIndex and ServerTokens
// of conf/httpd.conf.
func httpdProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	version := getenv(hc, "HTTPD_VERSION")
	if version == "" {
		version = "2.4.65"
	}
	conf := "/usr/local/apache2/conf/httpd.conf"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-v" || a == "-V":
			fmt.Fprintf(hc.Stdout, "Server version: Apache/%s (Unix)\nServer built:   built into Tabernetes (Go, js/wasm)\n", version)
			return nil
		case a == "-t" || a == "configtest":
			fmt.Fprintln(hc.Stderr, "Syntax OK")
			return nil
		case a == "-k" && i+1 < len(args), a == "graceful" || a == "restart" || a == "stop" || a == "start":
			// The running server reads the files on every request.
			return nil
		case a == "-f" && i+1 < len(args):
			i++
			conf = args[i]
		case a == "-D" && i+1 < len(args):
			i++
		}
	}
	real := containerFS(ctx)
	listens := []string{}
	docRoot := "/usr/local/apache2/htdocs"
	index := []string{"index.html"}
	tokens := "Full"
	if b, err := os.ReadFile(real(conf)); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || strings.HasPrefix(f[0], "#") {
				continue
			}
			switch strings.ToLower(f[0]) {
			case "listen":
				listens = append(listens, f[1])
			case "documentroot":
				docRoot = strings.Trim(f[1], `"`)
			case "directoryindex":
				index = f[1:]
			case "servertokens":
				tokens = f[1]
			}
		}
	}
	if len(listens) == 0 {
		listens = []string{"80"}
	}
	server := "Apache/" + version + " (Unix)"
	switch strings.ToLower(tokens) {
	case "prod", "productonly":
		server = "Apache"
	case "major":
		server = "Apache/" + strings.SplitN(version, ".", 2)[0]
	case "minor":
		server = "Apache/" + strings.Join(strings.SplitN(version, ".", 3)[:2], ".")
	case "min", "minimal":
		server = "Apache/" + version
	}
	var addrs []string
	for _, l := range listens {
		if _, err := strconv.Atoi(l); err == nil {
			l = ":" + l
		}
		addrs = append(addrs, l)
	}
	ip := podIP(ctx)
	out := &syncWriter{w: hc.Stdout}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("Server", server)
		httpdServe(sw, r, real, docRoot, index)
		size := "-"
		if sw.size > 0 {
			size = strconv.Itoa(sw.size)
		}
		user := "-"
		if u, _, ok := r.BasicAuth(); ok {
			user = u
		}
		fmt.Fprintf(out, "%s - %s [%s] \"%s %s %s\" %d %s\n", remoteHost(r), user, time.Now().UTC().Format("02/Jan/2006:15:04:05 -0700"), r.Method, r.URL.RequestURI(), r.Proto, sw.code(), size)
	})
	fmt.Fprintf(hc.Stderr, "AH00558: httpd: Could not reliably determine the server's fully qualified domain name, using %s. Set the 'ServerName' directive globally to suppress this message\n", ip)
	fmt.Fprintf(hc.Stderr, "AH00558: httpd: Could not reliably determine the server's fully qualified domain name, using %s. Set the 'ServerName' directive globally to suppress this message\n", ip)
	fmt.Fprintf(hc.Stderr, "[%s] [mpm_event:notice] [pid 1:tid 1] AH00489: Apache/%s (Unix) configured -- resuming normal operations\n", apacheTime(time.Now().UTC()), version)
	fmt.Fprintf(hc.Stderr, "[%s] [core:notice] [pid 1:tid 1] AH00094: Command line: 'httpd -D FOREGROUND'\n", apacheTime(time.Now().UTC()))
	err := serveHTTP(ctx, hc, "httpd", h, addrs...)
	if err == nil {
		fmt.Fprintf(hc.Stderr, "[%s] [mpm_event:notice] [pid 1:tid 1] AH00491: caught SIGTERM, shutting down\n", apacheTime(time.Now().UTC()))
	}
	return err
}

func apacheTime(t time.Time) string {
	return t.Format("Mon Jan 02 15:04:05.000000 2006")
}

func httpdError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	title := strconv.Itoa(code) + " " + http.StatusText(code)
	body := fmt.Sprintf("<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>%s</title>\n</head><body>\n<h1>%s</h1>\n<p>%s</p>\n</body></html>\n", title, http.StatusText(code), msg)
	w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		io.WriteString(w, body)
	}
}

func httpdServe(w http.ResponseWriter, r *http.Request, real func(string) string, docRoot string, index []string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Allow", "GET,POST,OPTIONS,HEAD")
		w.WriteHeader(http.StatusOK)
		return
	default:
		httpdError(w, r, http.StatusMethodNotAllowed, fmt.Sprintf("The requested method %s is not allowed for this URL.", html.EscapeString(r.Method)))
		return
	}
	uri := ngxNormalize(r.URL.Path)
	file := strings.TrimSuffix(docRoot, "/") + uri
	fi, err := os.Stat(real(file))
	if err != nil {
		httpdError(w, r, http.StatusNotFound, "The requested URL was not found on this server.")
		return
	}
	if fi.IsDir() {
		if !strings.HasSuffix(uri, "/") {
			loc := "http://" + r.Host + (&url.URL{Path: uri + "/"}).EscapedPath()
			w.Header().Set("Location", loc)
			httpdError(w, r, http.StatusMovedPermanently, fmt.Sprintf("The document has moved <a href=\"%s\">here</a>.", html.EscapeString(loc)))
			return
		}
		for _, idx := range index {
			if ifi, err := os.Stat(real(file + idx)); err == nil && ifi.Mode().IsRegular() {
				file, fi = file+idx, ifi
				goto serve
			}
		}
		httpdIndex(w, r, uri, real(file))
		return
	}
serve:
	f, err := os.Open(real(file))
	if err != nil {
		httpdError(w, r, http.StatusForbidden, "You don't have permission to access this resource.")
		return
	}
	defer f.Close()
	if ct := mime.TypeByExtension(path.Ext(file)); ct != "" {
		w.Header().Set("Content-Type", strings.TrimSuffix(ct, "; charset=utf-8"))
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, fi.Size(), fi.ModTime().UnixMicro()))
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, file, fi.ModTime(), f)
}

func httpdIndex(w http.ResponseWriter, r *http.Request, uri, real string) {
	entries, err := os.ReadDir(real)
	if err != nil {
		httpdError(w, r, http.StatusForbidden, "You don't have permission to access this resource.")
		return
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 3.2 Final//EN\">\n<html>\n <head>\n  <title>Index of %s</title>\n </head>\n <body>\n<h1>Index of %s</h1>\n<ul>", html.EscapeString(strings.TrimSuffix(uri, "/")+"/"), html.EscapeString(strings.TrimSuffix(uri, "/")+"/"))
	if uri != "/" {
		b.WriteString("<li><a href=\"../\"> Parent Directory</a></li>\n")
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if fi, err := os.Stat(filepath.Join(real, name)); err == nil && fi.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "<li><a href=\"%s\"> %s</a></li>\n", url.PathEscape(strings.TrimSuffix(name, "/"))+strings.TrimPrefix(name, strings.TrimSuffix(name, "/")), html.EscapeString(name))
	}
	b.WriteString("</ul>\n</body></html>\n")
	w.Header().Set("Content-Type", "text/html;charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(b.Bytes())
	}
}

// nginxdemos/hello is nginx with a page that names the server that answered:
// with several replicas behind a Service, every request can show another
// pod. The plain-text tag answers in plain text.
const nginxHelloPlain = `server {
    listen 80;
    listen [::]:80;

    location / {
        default_type text/plain;
        expires -1;
        return 200 'Server address: $server_addr:$server_port\nServer name: $hostname\nDate: $time_local\nURI: $request_uri\nRequest ID: $request_id\n';
    }
}
`

const nginxHelloHTML = `server {
    listen 80;
    listen [::]:80;

    location / {
        default_type text/html;
        expires -1;
        return 200 '<!DOCTYPE html>
<html>
<head>
<title>Hello World</title>
<style>
body { margin: 0; padding: 0; font-family: Tahoma, Verdana, Arial, sans-serif; }
#center { width: 420px; margin: 80px auto; }
h1 { color: #009639; }
td { padding: 2px 12px 2px 0; }
td:first-child { font-weight: bold; }
</style>
</head>
<body>
<div id="center">
<h1>Hello World</h1>
<table>
<tr><td>Server&nbsp;address:</td><td>$server_addr:$server_port</td></tr>
<tr><td>Server&nbsp;name:</td><td>$hostname</td></tr>
<tr><td>Date:</td><td>$time_local</td></tr>
<tr><td>URI:</td><td>$request_uri</td></tr>
<tr><td>Request&nbsp;ID:</td><td>$request_id</td></tr>
</table>
</div>
</body>
</html>
';
    }
}
`

var nginxHelloFiles = mergeFiles(nginxFiles, map[string]string{
	"/etc/nginx/conf.d/default.conf": nginxHelloHTML,
})

// nginxHelloTag picks the plain-text page for the plain-text tag.
func nginxHelloTag(img *builtinImage, tag string) {
	if !strings.HasPrefix(tag, "plain-text") {
		return
	}
	img.files = mergeFiles(img.files, map[string]string{"/etc/nginx/conf.d/default.conf": nginxHelloPlain})
}
