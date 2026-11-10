package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// openssl built on crypto/x509 and friends, enough for the CertificateSigningRequest
// exercise: version, genrsa, genpkey, ecparam, req (-new, -x509, -text, -verify,
// -subj, -addext), x509 (-text, -subject, -req signing with a CA, -signkey, ...),
// pkey/rsa/ec, base64/enc -base64, rand, dgst (and sha256 etc. as commands) and
// verify. PEM only (DER for certificates and requests with -inform/-outform).
// Output follows OpenSSL 3; passphrases, ciphers and s_client are not available.
func init() { coreutils["openssl"] = opensslCmd }

const sslVersion = "OpenSSL 3.0.13 30 Jan 2024 (Library: OpenSSL 3.0.13 30 Jan 2024)"

const sslHelp = `Standard commands
base64   dgst     ec       ecparam  enc      genpkey  genrsa   pkey
rand     req      rsa      verify   version  x509

Message Digest commands
md5      sha1     sha256   sha384   sha512

Not supported here: s_client and the other network, CA and cipher commands.
`

type ssl struct {
	ctx context.Context
	hc  interp.HandlerContext
	cmd string
}

type sslFlags struct {
	v     map[string][]string
	pos   []string
	order []string
}

func (f *sslFlags) b(k string) bool { _, ok := f.v[k]; return ok }
func (f *sslFlags) s(k string) string {
	if l := f.v[k]; len(l) > 0 {
		return l[len(l)-1]
	}
	return ""
}

var sslValued = map[string]bool{}

func init() {
	for _, k := range strings.Fields("in out key keyout subj newkey pkeyopt algorithm name days addext CA CAkey CAserial set_serial signkey extfile extensions inform outform passin passout CAfile config untrusted hmac copy_extensions reqexts md pass keyform CAform rand") {
		sslValued[k] = true
	}
}

func parseSSLFlags(cmd string, args []string) (*sslFlags, error) {
	f := &sslFlags{v: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' {
			f.pos = append(f.pos, a)
			continue
		}
		k := strings.TrimPrefix(a[1:], "-")
		if sslValued[k] {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s: option -%s needs a value", cmd, k)
			}
			f.v[k] = append(f.v[k], args[i+1])
			i++
		} else {
			f.v[k] = f.v[k]
		}
		f.order = append(f.order, k)
	}
	return f, nil
}

func opensslCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) == 0 {
		args = []string{"help"}
	}
	s := &ssl{ctx: ctx, hc: hc, cmd: args[0]}
	rest := args[1:]
	switch s.cmd {
	case "help", "-help", "--help":
		io.WriteString(hc.Stdout, sslHelp)
		return nil
	case "s_client", "s_server", "s_time":
		return fail(hc, 1, "openssl: %s is not supported in the browser", s.cmd)
	case "sha1", "sha256", "sha384", "sha512", "md5":
		rest = append([]string{"-" + s.cmd}, rest...)
		s.cmd = "dgst"
	}
	var run func(*sslFlags) error
	switch s.cmd {
	case "version":
		run = func(*sslFlags) error { fmt.Fprintln(hc.Stdout, sslVersion); return nil }
	case "genrsa":
		run = s.genrsa
	case "genpkey":
		run = s.genpkey
	case "ecparam":
		run = s.ecparam
	case "req":
		run = s.req
	case "x509":
		run = s.x509
	case "pkey", "rsa", "ec":
		run = s.pkey
	case "base64", "enc":
		run = s.enc
	case "rand":
		run = s.rand
	case "dgst":
		run = s.dgst
	case "verify":
		run = s.verify
	default:
		return fail(hc, 1, "Invalid command '%s'; type \"help\" for a list.", s.cmd)
	}
	f, err := parseSSLFlags(s.cmd, rest)
	if err == nil {
		err = run(f)
	}
	if err == nil {
		return nil
	}
	if _, ok := err.(interp.ExitStatus); ok {
		return err
	}
	return fail(hc, 1, "%v", err)
}

func (s *ssl) read(path string) ([]byte, error) {
	if path == "" || path == "-" {
		if s.hc.Stdin == nil {
			return nil, nil
		}
		return io.ReadAll(s.hc.Stdin)
	}
	b, err := os.ReadFile(abs(s.ctx, s.hc, path))
	if err != nil {
		return nil, fmt.Errorf("%s: Can't open %q for reading: %v", s.cmd, path, unwrap(err))
	}
	return b, nil
}

func (s *ssl) write(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		_, err := s.hc.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(abs(s.ctx, s.hc, path), data, perm); err != nil {
		return fmt.Errorf("%s: Can't open %q for writing: %v", s.cmd, path, unwrap(err))
	}
	return nil
}

// ---- keys ----

var sslCurves = []struct {
	names   []string
	c       elliptic.Curve
	sn      string
	nist    string
	oid     asn1.ObjectIdentifier
	bitSize int
}{
	{[]string{"p-256", "prime256v1", "secp256r1"}, elliptic.P256(), "prime256v1", "P-256", asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}, 256},
	{[]string{"p-384", "secp384r1"}, elliptic.P384(), "secp384r1", "P-384", asn1.ObjectIdentifier{1, 3, 132, 0, 34}, 384},
	{[]string{"p-521", "secp521r1"}, elliptic.P521(), "secp521r1", "P-521", asn1.ObjectIdentifier{1, 3, 132, 0, 35}, 521},
}

func sslCurve(name string) (int, error) {
	for i, c := range sslCurves {
		for _, n := range c.names {
			if strings.EqualFold(n, name) {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unknown curve name (%s)", name)
}

func sslCurveOf(c elliptic.Curve) int {
	for i, x := range sslCurves {
		if x.c == c {
			return i
		}
	}
	return 0
}

// sslGenKey makes a key: alg is rsa, ec or ed25519, opts are the -pkeyopt values.
func sslGenKey(alg string, bits int, curve string) (crypto.Signer, error) {
	switch strings.ToLower(alg) {
	case "rsa":
		if bits == 0 {
			bits = 2048
		}
		if bits < 512 || bits > 16384 {
			return nil, fmt.Errorf("invalid RSA key size %d (512 to 16384)", bits)
		}
		return rsa.GenerateKey(rand.Reader, bits)
	case "ec":
		if curve == "" {
			curve = "P-256"
		}
		i, err := sslCurve(curve)
		if err != nil {
			return nil, err
		}
		return ecdsa.GenerateKey(sslCurves[i].c, rand.Reader)
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	return nil, fmt.Errorf("unknown algorithm %q", alg)
}

// sslPkeyopts reads rsa_keygen_bits and ec_paramgen_curve from -pkeyopt values.
func sslPkeyopts(l []string) (bits int, curve string) {
	for _, o := range l {
		k, v, _ := strings.Cut(o, ":")
		switch k {
		case "rsa_keygen_bits":
			bits, _ = strconv.Atoi(v)
		case "ec_paramgen_curve":
			curve = v
		}
	}
	return
}

func sslKeyPEM(k crypto.Signer, traditional bool) []byte {
	var typ string
	var der []byte
	switch kk := k.(type) {
	case *rsa.PrivateKey:
		if traditional {
			typ, der = "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(kk)
		}
	case *ecdsa.PrivateKey:
		if traditional {
			typ = "EC PRIVATE KEY"
			der, _ = x509.MarshalECPrivateKey(kk)
		}
	}
	if der == nil {
		typ = "PRIVATE KEY"
		der, _ = x509.MarshalPKCS8PrivateKey(k)
	}
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func sslPubPEM(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func sslLoadKey(data []byte) (crypto.Signer, error) {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, errors.New("unable to load private key (no PEM private key found)")
		}
		var k any
		var err error
		switch b.Type {
		case "PRIVATE KEY":
			k, err = x509.ParsePKCS8PrivateKey(b.Bytes)
		case "RSA PRIVATE KEY":
			k, err = x509.ParsePKCS1PrivateKey(b.Bytes)
		case "EC PRIVATE KEY":
			k, err = x509.ParseECPrivateKey(b.Bytes)
		case "ENCRYPTED PRIVATE KEY":
			return nil, errors.New("encrypted private keys are not supported")
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("unable to load private key: %v", err)
		}
		if sg, ok := k.(crypto.Signer); ok {
			return sg, nil
		}
		return nil, errors.New("unsupported private key type")
	}
}

func sslLoadPub(data []byte) (crypto.PublicKey, error) {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, errors.New("unable to load public key")
		}
		switch b.Type {
		case "PUBLIC KEY":
			return x509.ParsePKIXPublicKey(b.Bytes)
		case "RSA PUBLIC KEY":
			return x509.ParsePKCS1PublicKey(b.Bytes)
		}
	}
}

func (s *ssl) genrsa(f *sslFlags) error {
	bits := 2048
	if len(f.pos) > 0 {
		n, err := strconv.Atoi(f.pos[len(f.pos)-1])
		if err != nil {
			return fmt.Errorf("genrsa: invalid key size %q", f.pos[len(f.pos)-1])
		}
		bits = n
	}
	k, err := sslGenKey("rsa", bits, "")
	if err != nil {
		return fmt.Errorf("genrsa: %v", err)
	}
	return s.write(f.s("out"), sslKeyPEM(k, f.b("traditional")), 0o600)
}

func (s *ssl) genpkey(f *sslFlags) error {
	alg := f.s("algorithm")
	if alg == "" {
		return errors.New("genpkey: no -algorithm given")
	}
	bits, curve := sslPkeyopts(f.v["pkeyopt"])
	k, err := sslGenKey(alg, bits, curve)
	if err != nil {
		return fmt.Errorf("genpkey: %v", err)
	}
	return s.write(f.s("out"), sslKeyPEM(k, false), 0o600)
}

func (s *ssl) ecparam(f *sslFlags) error {
	name := f.s("name")
	if name == "" {
		return errors.New("ecparam: no -name given")
	}
	ci, err := sslCurve(name)
	if err != nil {
		return fmt.Errorf("ecparam: %v", err)
	}
	var out []byte
	if !f.b("noout") {
		der, _ := asn1.Marshal(sslCurves[ci].oid)
		out = pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: der})
	}
	if f.b("genkey") {
		k, err := ecdsa.GenerateKey(sslCurves[ci].c, rand.Reader)
		if err != nil {
			return err
		}
		out = append(out, sslKeyPEM(k, true)...)
	}
	return s.write(f.s("out"), out, 0o600)
}

// pkey handles pkey, rsa and ec: re-encode a key, print its public half or text.
func (s *ssl) pkey(f *sslFlags) error {
	data, err := s.read(f.s("in"))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	var pub crypto.PublicKey
	var priv crypto.Signer
	if f.b("pubin") {
		if pub, err = sslLoadPub(data); err != nil {
			return fmt.Errorf("%s: %v", s.cmd, err)
		}
	} else {
		if priv, err = sslLoadKey(data); err != nil {
			return fmt.Errorf("%s: %v", s.cmd, err)
		}
		pub = priv.Public()
	}
	if f.b("text") {
		if priv != nil {
			fmt.Fprintf(&buf, "%s\n", sslPrivHeader(priv))
		}
		buf.WriteString(sslPubBody(pub, ""))
	}
	if f.b("modulus") {
		if r, ok := pub.(*rsa.PublicKey); ok {
			fmt.Fprintf(&buf, "Modulus=%X\n", r.N)
		}
	}
	if !f.b("noout") {
		switch {
		case f.b("pubout") || priv == nil:
			p, err := sslPubPEM(pub)
			if err != nil {
				return err
			}
			buf.Write(p)
		default:
			buf.Write(sslKeyPEM(priv, f.b("traditional") || s.cmd == "ec"))
		}
	}
	return s.write(f.s("out"), buf.Bytes(), 0o600)
}

func sslPrivHeader(k crypto.Signer) string {
	switch kk := k.(type) {
	case *rsa.PrivateKey:
		return fmt.Sprintf("Private-Key: (%d bit, %d primes)", kk.N.BitLen(), len(kk.Primes))
	case *ecdsa.PrivateKey:
		return fmt.Sprintf("Private-Key: (%d bit)", kk.Curve.Params().BitSize)
	}
	return "ED25519 Private-Key:"
}

// ---- names and extensions ----

var sslAttrs = []struct {
	short string
	oid   asn1.ObjectIdentifier
	alias string
}{
	{"C", asn1.ObjectIdentifier{2, 5, 4, 6}, "countryName"},
	{"ST", asn1.ObjectIdentifier{2, 5, 4, 8}, "stateOrProvinceName"},
	{"L", asn1.ObjectIdentifier{2, 5, 4, 7}, "localityName"},
	{"O", asn1.ObjectIdentifier{2, 5, 4, 10}, "organizationName"},
	{"OU", asn1.ObjectIdentifier{2, 5, 4, 11}, "organizationalUnitName"},
	{"CN", asn1.ObjectIdentifier{2, 5, 4, 3}, "commonName"},
	{"serialNumber", asn1.ObjectIdentifier{2, 5, 4, 5}, ""},
	{"street", asn1.ObjectIdentifier{2, 5, 4, 9}, "streetAddress"},
	{"title", asn1.ObjectIdentifier{2, 5, 4, 12}, ""},
	{"SN", asn1.ObjectIdentifier{2, 5, 4, 4}, "surname"},
	{"GN", asn1.ObjectIdentifier{2, 5, 4, 42}, "givenName"},
	{"DC", asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 25}, "domainComponent"},
	{"UID", asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}, "userId"},
	{"emailAddress", asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}, ""},
}

// sslParseSubj turns "/CN=jane/O=dev/O=x" into an RDN sequence in the given order.
func sslParseSubj(subj string) (pkix.RDNSequence, error) {
	if !strings.HasPrefix(subj, "/") {
		return nil, fmt.Errorf("req: invalid subject %q (must start with /)", subj)
	}
	var parts []string
	var cur strings.Builder
	for i := 1; i < len(subj); i++ {
		switch c := subj[i]; {
		case c == '\\' && i+1 < len(subj):
			i++
			cur.WriteByte(subj[i])
		case c == '/':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	var seq pkix.RDNSequence
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("req: invalid subject part %q (expected key=value)", p)
		}
		var oid asn1.ObjectIdentifier
		for _, a := range sslAttrs {
			if strings.EqualFold(k, a.short) || (a.alias != "" && strings.EqualFold(k, a.alias)) {
				oid = a.oid
			}
		}
		if oid == nil {
			return nil, fmt.Errorf("req: unknown subject attribute %q", k)
		}
		if v == "" {
			continue
		}
		tag := asn1.TagUTF8String
		if oid.Equal(sslAttrs[0].oid) {
			tag = asn1.TagPrintableString
		} else if oid.Equal(sslAttrs[11].oid) || oid.Equal(sslAttrs[13].oid) {
			tag = asn1.TagIA5String
		}
		seq = append(seq, pkix.RelativeDistinguishedNameSET{{Type: oid, Value: asn1.RawValue{Tag: tag, Bytes: []byte(v)}}})
	}
	return seq, nil
}

// sslName prints a DER name the way OpenSSL 3 does: "CN = jane, O = dev".
func sslName(raw []byte) string {
	var seq pkix.RDNSequence
	if _, err := asn1.Unmarshal(raw, &seq); err != nil {
		return ""
	}
	var rdns []string
	for _, rdn := range seq {
		var as []string
		for _, a := range rdn {
			name := a.Type.String()
			for _, x := range sslAttrs {
				if x.oid.Equal(a.Type) {
					name = x.short
				}
			}
			as = append(as, fmt.Sprintf("%s = %v", name, a.Value))
		}
		rdns = append(rdns, strings.Join(as, " + "))
	}
	return strings.Join(rdns, ", ")
}

// sslExts holds the extensions we know how to put in a certificate.
type sslExts struct {
	dns     []string
	ips     []net.IP
	emails  []string
	uris    []*url.URL
	ku      x509.KeyUsage
	eku     []x509.ExtKeyUsage
	hasBC   bool
	ca      bool
	pathlen int
}

var (
	sslKU = map[string]x509.KeyUsage{"digitalsignature": x509.KeyUsageDigitalSignature, "nonrepudiation": x509.KeyUsageContentCommitment,
		"contentcommitment": x509.KeyUsageContentCommitment, "keyencipherment": x509.KeyUsageKeyEncipherment, "dataencipherment": x509.KeyUsageDataEncipherment,
		"keyagreement": x509.KeyUsageKeyAgreement, "keycertsign": x509.KeyUsageCertSign, "crlsign": x509.KeyUsageCRLSign,
		"encipheronly": x509.KeyUsageEncipherOnly, "decipheronly": x509.KeyUsageDecipherOnly}
	sslEKU = map[string]x509.ExtKeyUsage{"serverauth": x509.ExtKeyUsageServerAuth, "clientauth": x509.ExtKeyUsageClientAuth,
		"codesigning": x509.ExtKeyUsageCodeSigning, "emailprotection": x509.ExtKeyUsageEmailProtection,
		"timestamping": x509.ExtKeyUsageTimeStamping, "ocspsigning": x509.ExtKeyUsageOCSPSigning, "anyextendedkeyusage": x509.ExtKeyUsageAny}
)

func (e *sslExts) add(name, val string) error {
	val = strings.TrimSpace(val)
	for strings.HasPrefix(val, "critical") {
		val = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(val, "critical"), ","))
	}
	for _, item := range strings.Split(val, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, _ := strings.Cut(item, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch strings.ToLower(name) {
		case "subjectaltname":
			switch strings.ToLower(k) {
			case "dns":
				e.dns = append(e.dns, v)
			case "ip":
				ip := net.ParseIP(v)
				if ip == nil {
					return fmt.Errorf("invalid IP address %q in subjectAltName", v)
				}
				e.ips = append(e.ips, ip)
			case "email":
				e.emails = append(e.emails, v)
			case "uri":
				u, err := url.Parse(v)
				if err != nil {
					return err
				}
				e.uris = append(e.uris, u)
			default:
				return fmt.Errorf("unsupported subjectAltName type %q", k)
			}
		case "basicconstraints":
			e.hasBC = true
			switch strings.ToLower(k) {
			case "ca":
				e.ca = strings.EqualFold(v, "true")
			case "pathlen":
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("invalid pathlen %q", v)
				}
				e.pathlen = n + 1
			}
		case "keyusage":
			u, ok := sslKU[strings.ToLower(k)]
			if !ok {
				return fmt.Errorf("unknown key usage %q", k)
			}
			e.ku |= u
		case "extendedkeyusage":
			u, ok := sslEKU[strings.ToLower(k)]
			if !ok {
				return fmt.Errorf("unknown extended key usage %q", k)
			}
			e.eku = append(e.eku, u)
		}
	}
	return nil
}

// addExt adds a "name=value" line.
func (e *sslExts) addLine(l string) error {
	k, v, ok := strings.Cut(l, "=")
	if !ok {
		return fmt.Errorf("invalid extension %q (expected name=value)", l)
	}
	return e.add(strings.TrimSpace(k), v)
}

func (e *sslExts) apply(t *x509.Certificate) {
	t.DNSNames, t.IPAddresses, t.EmailAddresses, t.URIs = e.dns, e.ips, e.emails, e.uris
	t.KeyUsage, t.ExtKeyUsage = e.ku, e.eku
	if e.hasBC {
		t.BasicConstraintsValid, t.IsCA = true, e.ca
		t.MaxPathLen, t.MaxPathLenZero = -1, false
		if e.pathlen > 0 {
			t.MaxPathLen, t.MaxPathLenZero = e.pathlen-1, e.pathlen == 1
		}
	}
}

// extfile reads "name = value" lines of the section (or of the top when sec is empty).
func sslExtLines(data, sec string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		if l[0] == '[' {
			cur = strings.TrimSpace(strings.Trim(l, "[]"))
			continue
		}
		if cur == sec {
			out = append(out, l)
		}
	}
	return out
}

func sslSigAlg(k crypto.Signer, f *sslFlags) x509.SignatureAlgorithm {
	h := 256
	if f.b("sha384") {
		h = 384
	} else if f.b("sha512") {
		h = 512
	}
	switch k.(type) {
	case *rsa.PrivateKey:
		return map[int]x509.SignatureAlgorithm{256: x509.SHA256WithRSA, 384: x509.SHA384WithRSA, 512: x509.SHA512WithRSA}[h]
	case *ecdsa.PrivateKey:
		return map[int]x509.SignatureAlgorithm{256: x509.ECDSAWithSHA256, 384: x509.ECDSAWithSHA384, 512: x509.ECDSAWithSHA512}[h]
	}
	return x509.PureEd25519
}

func sslSerial() *big.Int {
	for {
		n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
		if n.Sign() > 0 {
			return n
		}
	}
}

func sslSKI(pub crypto.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	var spki struct {
		Algo pkix.AlgorithmIdentifier
		Bits asn1.BitString
	}
	asn1.Unmarshal(der, &spki)
	h := sha1.Sum(spki.Bits.Bytes)
	return h[:]
}

func sslDays(f *sslFlags) (time.Time, time.Time, error) {
	d := 30
	if v := f.s("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("%s: invalid -days %q", "openssl", v)
		}
		d = n
	}
	now := time.Now().UTC().Truncate(time.Second)
	return now, now.AddDate(0, 0, d), nil
}

func sslEncode(f *sslFlags, typ string, der []byte) []byte {
	if strings.EqualFold(f.s("outform"), "DER") {
		return der
	}
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// sslBlockDER finds the first PEM block of one of the types, or takes data as DER.
func sslBlockDER(data []byte, types ...string) []byte {
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		for _, t := range types {
			if b.Type == t {
				return b.Bytes
			}
		}
	}
	if bytes.Contains(data, []byte("-----BEGIN")) {
		return nil
	}
	return data
}

func sslParseCert(data []byte) (*x509.Certificate, error) {
	der := sslBlockDER(data, "CERTIFICATE", "TRUSTED CERTIFICATE", "X509 CERTIFICATE")
	if der == nil {
		return nil, errors.New("unable to load certificate (no PEM CERTIFICATE found)")
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("unable to load certificate: %v", err)
	}
	return c, nil
}

func sslParseCSR(data []byte) (*x509.CertificateRequest, error) {
	der := sslBlockDER(data, "CERTIFICATE REQUEST", "NEW CERTIFICATE REQUEST")
	if der == nil {
		return nil, errors.New("unable to load certificate request (no PEM CERTIFICATE REQUEST found)")
	}
	c, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("unable to load certificate request: %v", err)
	}
	return c, nil
}

// ---- req ----

func (s *ssl) req(f *sslFlags) error {
	if !(f.b("new") || f.b("newkey") || f.b("x509")) {
		data, err := s.read(f.s("in"))
		if err != nil {
			return err
		}
		csr, err := sslParseCSR(data)
		if err != nil {
			return fmt.Errorf("req: %v", err)
		}
		return s.reqOut(f, csr, nil)
	}
	subj := f.s("subj")
	if subj == "" {
		return errors.New("req: no -subj given (interactive prompts are not supported)")
	}
	seq, err := sslParseSubj(subj)
	if err != nil {
		return err
	}
	rawSubj, _ := asn1.Marshal(seq)
	var key crypto.Signer
	if kf := f.s("key"); kf != "" {
		data, err := s.read(kf)
		if err != nil {
			return err
		}
		if key, err = sslLoadKey(data); err != nil {
			return fmt.Errorf("req: %v", err)
		}
	} else if spec := f.s("newkey"); spec != "" {
		alg, arg, _ := strings.Cut(spec, ":")
		bits, curve := sslPkeyopts(f.v["pkeyopt"])
		if n, err := strconv.Atoi(arg); err == nil {
			bits = n
		} else if arg != "" {
			curve = arg
		}
		if key, err = sslGenKey(alg, bits, curve); err != nil {
			return fmt.Errorf("req: %v", err)
		}
		kout := f.s("keyout")
		if kout == "" {
			kout = "privkey.pem"
		}
		if err := s.write(kout, sslKeyPEM(key, false), 0o600); err != nil {
			return err
		}
	} else {
		return errors.New("req: no -key or -newkey given")
	}
	var ex sslExts
	for _, l := range f.v["addext"] {
		if err := ex.addLine(l); err != nil {
			return fmt.Errorf("req: %v", err)
		}
	}
	if f.b("x509") {
		nb, na, err := sslDays(f)
		if err != nil {
			return err
		}
		serial := sslSerial()
		if v := f.s("set_serial"); v != "" {
			var ok bool
			if serial, ok = new(big.Int).SetString(v, 0); !ok {
				return fmt.Errorf("req: invalid -set_serial %q", v)
			}
		}
		ski := sslSKI(key.Public())
		t := &x509.Certificate{SerialNumber: serial, RawSubject: rawSubj, NotBefore: nb, NotAfter: na,
			SignatureAlgorithm: sslSigAlg(key, f), SubjectKeyId: ski, AuthorityKeyId: ski,
			BasicConstraintsValid: true, IsCA: true, MaxPathLen: -1}
		ex.apply(t)
		der, err := x509.CreateCertificate(rand.Reader, t, t, key.Public(), key)
		if err != nil {
			return fmt.Errorf("req: %v", err)
		}
		cert, _ := x509.ParseCertificate(der)
		return s.certOut(f, cert, "req")
	}
	if len(ex.ips)+len(ex.dns)+len(ex.emails)+len(ex.uris) == 0 && (ex.hasBC || ex.ku != 0 || len(ex.eku) > 0) {
		return errors.New("req: only subjectAltName is supported in -addext of a request")
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{RawSubject: rawSubj, SignatureAlgorithm: sslSigAlg(key, f),
		DNSNames: ex.dns, IPAddresses: ex.ips, EmailAddresses: ex.emails, URIs: ex.uris}, key)
	if err != nil {
		return fmt.Errorf("req: %v", err)
	}
	csr, _ := x509.ParseCertificateRequest(der)
	return s.reqOut(f, csr, der)
}

func (s *ssl) reqOut(f *sslFlags, csr *x509.CertificateRequest, der []byte) error {
	if der == nil {
		der = csr.Raw
	}
	var buf bytes.Buffer
	for _, k := range f.order {
		switch k {
		case "text":
			buf.WriteString(sslCSRText(csr))
		case "subject":
			fmt.Fprintf(&buf, "subject=%s\n", sslName(csr.RawSubject))
		case "verify":
			if err := csr.CheckSignature(); err != nil {
				return fmt.Errorf("req: signature verification failed: %v", err)
			}
			buf.WriteString("verify OK\n")
		}
	}
	if !f.b("noout") {
		buf.Write(sslEncode(f, "CERTIFICATE REQUEST", der))
	}
	return s.write(f.s("out"), buf.Bytes(), 0o644)
}

func (s *ssl) certOut(f *sslFlags, c *x509.Certificate, cmd string) error {
	var buf bytes.Buffer
	if f.b("text") {
		buf.WriteString(sslCertText(c))
	}
	if f.b("subject") {
		fmt.Fprintf(&buf, "subject=%s\n", sslName(c.RawSubject))
	}
	if !f.b("noout") {
		buf.Write(sslEncode(f, "CERTIFICATE", c.Raw))
	}
	return s.write(f.s("out"), buf.Bytes(), 0o644)
}

// ---- x509 ----

func (s *ssl) x509(f *sslFlags) error {
	data, err := s.read(f.s("in"))
	if err != nil {
		return err
	}
	var cert *x509.Certificate
	if f.b("req") {
		if cert, err = s.signReq(f, data); err != nil {
			return err
		}
	} else if cert, err = sslParseCert(data); err != nil {
		return fmt.Errorf("x509: %v", err)
	}
	var buf bytes.Buffer
	for _, k := range f.order {
		switch k {
		case "text":
			buf.WriteString(sslCertText(cert))
		case "subject":
			fmt.Fprintf(&buf, "subject=%s\n", sslName(cert.RawSubject))
		case "issuer":
			fmt.Fprintf(&buf, "issuer=%s\n", sslName(cert.RawIssuer))
		case "serial":
			fmt.Fprintf(&buf, "serial=%s\n", sslSerialHex(cert.SerialNumber))
		case "dates":
			fmt.Fprintf(&buf, "notBefore=%s\nnotAfter=%s\n", sslTime(cert.NotBefore), sslTime(cert.NotAfter))
		case "startdate":
			fmt.Fprintf(&buf, "notBefore=%s\n", sslTime(cert.NotBefore))
		case "enddate":
			fmt.Fprintf(&buf, "notAfter=%s\n", sslTime(cert.NotAfter))
		case "fingerprint":
			name, sum := "SHA1", sha1.Sum(cert.Raw)
			var d []byte = sum[:]
			if f.b("sha256") {
				s2 := sha256.Sum256(cert.Raw)
				name, d = "SHA256", s2[:]
			}
			fmt.Fprintf(&buf, "%s Fingerprint=%s\n", name, sslHexU(d))
		case "pubkey":
			p, err := sslPubPEM(cert.PublicKey)
			if err != nil {
				return err
			}
			buf.Write(p)
		case "modulus":
			if r, ok := cert.PublicKey.(*rsa.PublicKey); ok {
				fmt.Fprintf(&buf, "Modulus=%X\n", r.N)
			}
		}
	}
	if !f.b("noout") {
		buf.Write(sslEncode(f, "CERTIFICATE", cert.Raw))
	}
	return s.write(f.s("out"), buf.Bytes(), 0o644)
}

func sslSerialHex(n *big.Int) string {
	b := n.Bytes()
	if len(b) == 0 {
		b = []byte{0}
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// signReq signs the request of data with -CA/-CAkey or -signkey.
func (s *ssl) signReq(f *sslFlags, data []byte) (*x509.Certificate, error) {
	csr, err := sslParseCSR(data)
	if err != nil {
		return nil, fmt.Errorf("x509: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("x509: certificate request signature failure: %v", err)
	}
	var parent *x509.Certificate
	var key crypto.Signer
	caPath := f.s("CA")
	switch {
	case caPath != "":
		cad, err := s.read(caPath)
		if err != nil {
			return nil, err
		}
		if parent, err = sslParseCert(cad); err != nil {
			return nil, fmt.Errorf("x509: %v", err)
		}
		kd := cad
		if kf := f.s("CAkey"); kf != "" {
			if kd, err = s.read(kf); err != nil {
				return nil, err
			}
		}
		if key, err = sslLoadKey(kd); err != nil {
			return nil, fmt.Errorf("x509: CA key: %v", err)
		}
	case f.s("signkey") != "":
		kd, err := s.read(f.s("signkey"))
		if err != nil {
			return nil, err
		}
		if key, err = sslLoadKey(kd); err != nil {
			return nil, fmt.Errorf("x509: %v", err)
		}
	default:
		return nil, errors.New("x509: -req needs -CA and -CAkey, or -signkey")
	}
	nb, na, err := sslDays(f)
	if err != nil {
		return nil, err
	}
	serial, err := s.serial(f, caPath)
	if err != nil {
		return nil, err
	}
	var ex sslExts
	if sec, ef := f.s("extensions"), f.s("extfile"); ef != "" {
		ed, err := s.read(ef)
		if err != nil {
			return nil, err
		}
		for _, l := range sslExtLines(string(ed), sec) {
			if err := ex.addLine(l); err != nil {
				return nil, fmt.Errorf("x509: %v", err)
			}
		}
	}
	if strings.HasPrefix(f.s("copy_extensions"), "copy") {
		ex.dns, ex.ips, ex.emails, ex.uris = append(ex.dns, csr.DNSNames...), append(ex.ips, csr.IPAddresses...), append(ex.emails, csr.EmailAddresses...), append(ex.uris, csr.URIs...)
	}
	t := &x509.Certificate{SerialNumber: serial, RawSubject: csr.RawSubject, NotBefore: nb, NotAfter: na, SignatureAlgorithm: sslSigAlg(key, f)}
	ex.apply(t)
	if parent == nil {
		parent = t
	}
	der, err := x509.CreateCertificate(rand.Reader, t, parent, csr.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("x509: %v", err)
	}
	return x509.ParseCertificate(der)
}

// serial picks the serial: -set_serial, the CA serial file, or random.
func (s *ssl) serial(f *sslFlags, caPath string) (*big.Int, error) {
	if v := f.s("set_serial"); v != "" {
		n, ok := new(big.Int).SetString(v, 0)
		if !ok || n.Sign() < 0 {
			return nil, fmt.Errorf("x509: invalid -set_serial %q", v)
		}
		return n, nil
	}
	if caPath == "" {
		return sslSerial(), nil
	}
	sf := f.s("CAserial")
	if sf == "" {
		sf = strings.TrimSuffix(caPath, ".crt")
		sf = strings.TrimSuffix(sf, ".pem") + ".srl"
	}
	var n *big.Int
	if b, err := s.read(sf); err == nil {
		var ok bool
		if n, ok = new(big.Int).SetString(strings.TrimSpace(string(b)), 16); !ok {
			return nil, fmt.Errorf("x509: invalid serial file %s", sf)
		}
	} else if f.b("CAcreateserial") {
		n = new(big.Int).SetBytes(sslRand(8))
		n.SetBit(n, 63, 0)
		n.Add(n, big.NewInt(1))
	} else {
		return n, fmt.Errorf("x509: %s: no serial number file (use -CAcreateserial or -set_serial)", sf)
	}
	next := sslSerialHex(new(big.Int).Add(n, big.NewInt(1)))
	if len(next)%2 == 1 {
		next = "0" + next
	}
	if err := s.write(sf, []byte(next+"\n"), 0o644); err != nil {
		return nil, err
	}
	return n, nil
}

func sslRand(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// ---- text output ----

func sslHexU(b []byte) string {
	return strings.ToUpper(sslJoinHex(b, ":"))
}

func sslJoinHex(b []byte, sep string) string {
	p := make([]string, len(b))
	for i, c := range b {
		p[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(p, sep)
}

// sslHexLines prints b as colon separated lines of per bytes each.
func sslHexLines(b []byte, per int, ind string) string {
	var sb strings.Builder
	for i := 0; i < len(b); i += per {
		end := min(i+per, len(b))
		sb.WriteString(ind + sslJoinHex(b[i:end], ":"))
		if end < len(b) {
			sb.WriteByte(':')
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func sslTime(t time.Time) string { return t.UTC().Format("Jan _2 15:04:05 2006 GMT") }

func sslPubAlg(pub crypto.PublicKey) string {
	switch pub.(type) {
	case *rsa.PublicKey:
		return "rsaEncryption"
	case *ecdsa.PublicKey:
		return "id-ecPublicKey"
	case ed25519.PublicKey:
		return "ED25519"
	}
	return "unknown"
}

func sslPubBody(pub crypto.PublicKey, ind string) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		mod := k.N.Bytes()
		if len(mod) > 0 && mod[0]&0x80 != 0 {
			mod = append([]byte{0}, mod...)
		}
		return fmt.Sprintf("%sPublic-Key: (%d bit)\n%sModulus:\n%s%sExponent: %d (0x%x)\n", ind, k.N.BitLen(), ind, sslHexLines(mod, 15, ind+"    "), ind, k.E, k.E)
	case *ecdsa.PublicKey:
		c := sslCurves[sslCurveOf(k.Curve)]
		b, _ := k.Bytes()
		return fmt.Sprintf("%sPublic-Key: (%d bit)\n%spub:\n%s%sASN1 OID: %s\n%sNIST CURVE: %s\n", ind, c.bitSize, ind, sslHexLines(b, 15, ind+"    "), ind, c.sn, ind, c.nist)
	case ed25519.PublicKey:
		return fmt.Sprintf("%sED25519 Public-Key:\n%spub:\n%s", ind, ind, sslHexLines(k, 15, ind+"    "))
	}
	return ""
}

func sslSigName(a x509.SignatureAlgorithm) string {
	switch a {
	case x509.SHA256WithRSA:
		return "sha256WithRSAEncryption"
	case x509.SHA384WithRSA:
		return "sha384WithRSAEncryption"
	case x509.SHA512WithRSA:
		return "sha512WithRSAEncryption"
	case x509.SHA1WithRSA:
		return "sha1WithRSAEncryption"
	case x509.ECDSAWithSHA256:
		return "ecdsa-with-SHA256"
	case x509.ECDSAWithSHA384:
		return "ecdsa-with-SHA384"
	case x509.ECDSAWithSHA512:
		return "ecdsa-with-SHA512"
	case x509.PureEd25519:
		return "ED25519"
	case x509.SHA256WithRSAPSS:
		return "rsassaPss"
	}
	return a.String()
}

func sslSerialText(n *big.Int) string {
	if n.IsInt64() {
		return fmt.Sprintf("        Serial Number: %d (0x%x)\n", n, n)
	}
	b := n.Bytes()
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return "        Serial Number:\n" + sslHexLines(b, 15, "            ")
}

func sslCertText(c *x509.Certificate) string {
	var sb strings.Builder
	sb.WriteString("Certificate:\n    Data:\n")
	fmt.Fprintf(&sb, "        Version: %d (0x%x)\n", c.Version, c.Version-1)
	sb.WriteString(sslSerialText(c.SerialNumber))
	fmt.Fprintf(&sb, "        Signature Algorithm: %s\n        Issuer: %s\n        Validity\n            Not Before: %s\n            Not After : %s\n        Subject: %s\n",
		sslSigName(c.SignatureAlgorithm), sslName(c.RawIssuer), sslTime(c.NotBefore), sslTime(c.NotAfter), sslName(c.RawSubject))
	fmt.Fprintf(&sb, "        Subject Public Key Info:\n            Public Key Algorithm: %s\n%s", sslPubAlg(c.PublicKey), sslPubBody(c.PublicKey, "                "))
	if len(c.Extensions) > 0 {
		sb.WriteString("        X509v3 extensions:\n")
		for _, e := range c.Extensions {
			sb.WriteString(sslExtText(e, "            "))
		}
	}
	fmt.Fprintf(&sb, "    Signature Algorithm: %s\n    Signature Value:\n%s", sslSigName(c.SignatureAlgorithm), sslHexLines(c.Signature, 18, "        "))
	return sb.String()
}

func sslCSRText(r *x509.CertificateRequest) string {
	var sb strings.Builder
	sb.WriteString("Certificate Request:\n    Data:\n        Version: 1 (0x0)\n")
	fmt.Fprintf(&sb, "        Subject: %s\n        Subject Public Key Info:\n            Public Key Algorithm: %s\n%s", sslName(r.RawSubject), sslPubAlg(r.PublicKey), sslPubBody(r.PublicKey, "                "))
	sb.WriteString("        Attributes:\n")
	if len(r.Extensions) == 0 {
		sb.WriteString("            (none)\n")
	}
	sb.WriteString("            Requested Extensions:\n")
	for _, e := range r.Extensions {
		sb.WriteString(sslExtText(e, "                "))
	}
	fmt.Fprintf(&sb, "    Signature Algorithm: %s\n    Signature Value:\n%s", sslSigName(r.SignatureAlgorithm), sslHexLines(r.Signature, 18, "        "))
	return sb.String()
}

var sslKUNames = []string{"Digital Signature", "Non Repudiation", "Key Encipherment", "Data Encipherment", "Key Agreement", "Certificate Sign", "CRL Sign", "Encipher Only", "Decipher Only"}

var sslEKUNames = map[string]string{"1.3.6.1.5.5.7.3.1": "TLS Web Server Authentication", "1.3.6.1.5.5.7.3.2": "TLS Web Client Authentication",
	"1.3.6.1.5.5.7.3.3": "Code Signing", "1.3.6.1.5.5.7.3.4": "E-mail Protection", "1.3.6.1.5.5.7.3.8": "Time Stamping",
	"1.3.6.1.5.5.7.3.9": "OCSP Signing", "2.5.29.37.0": "Any Extended Key Usage"}

// sslExtText prints one extension in the layout of OpenSSL 3.
func sslExtText(e pkix.Extension, ind string) string {
	var name, body string
	in := ind + "    "
	switch e.Id.String() {
	case "2.5.29.15":
		name = "X509v3 Key Usage"
		var bs asn1.BitString
		asn1.Unmarshal(e.Value, &bs)
		var l []string
		for i, n := range sslKUNames {
			if bs.At(i) != 0 {
				l = append(l, n)
			}
		}
		body = in + strings.Join(l, ", ") + "\n"
	case "2.5.29.37":
		name = "X509v3 Extended Key Usage"
		var oids []asn1.ObjectIdentifier
		asn1.Unmarshal(e.Value, &oids)
		var l []string
		for _, o := range oids {
			if n, ok := sslEKUNames[o.String()]; ok {
				l = append(l, n)
			} else {
				l = append(l, o.String())
			}
		}
		body = in + strings.Join(l, ", ") + "\n"
	case "2.5.29.19":
		name = "X509v3 Basic Constraints"
		var bc struct {
			CA      bool `asn1:"optional"`
			PathLen int  `asn1:"optional,default:-1"`
		}
		asn1.Unmarshal(e.Value, &bc)
		v := "CA:FALSE"
		if bc.CA {
			v = "CA:TRUE"
		}
		if bc.PathLen >= 0 {
			v += fmt.Sprintf(", pathlen:%d", bc.PathLen)
		}
		body = in + v + "\n"
	case "2.5.29.14":
		name = "X509v3 Subject Key Identifier"
		var id []byte
		asn1.Unmarshal(e.Value, &id)
		body = in + sslHexU(id) + "\n"
	case "2.5.29.35":
		name = "X509v3 Authority Key Identifier"
		var aki struct {
			ID []byte `asn1:"optional,tag:0"`
		}
		asn1.Unmarshal(e.Value, &aki)
		body = in + sslHexU(aki.ID) + "\n"
	case "2.5.29.17":
		name = "X509v3 Subject Alternative Name"
		var names []asn1.RawValue
		asn1.Unmarshal(e.Value, &names)
		var l []string
		for _, n := range names {
			switch n.Tag {
			case 1:
				l = append(l, "email:"+string(n.Bytes))
			case 2:
				l = append(l, "DNS:"+string(n.Bytes))
			case 6:
				l = append(l, "URI:"+string(n.Bytes))
			case 7:
				l = append(l, "IP Address:"+net.IP(n.Bytes).String())
			default:
				l = append(l, fmt.Sprintf("<tag %d>", n.Tag))
			}
		}
		body = in + strings.Join(l, ", ") + "\n"
	default:
		name = e.Id.String()
		body = sslHexLines(e.Value, 16, in)
	}
	crit := ""
	if e.Critical {
		crit = "critical"
	}
	return fmt.Sprintf("%s%s: %s\n%s", ind, name, crit, body)
}

// ---- base64, rand, dgst, verify ----

func (s *ssl) enc(f *sslFlags) error {
	if s.cmd == "enc" && !f.b("base64") && !f.b("a") {
		return errors.New("enc: only -base64 is supported (ciphers are not available)")
	}
	data, err := s.read(f.s("in"))
	if err != nil {
		return err
	}
	if f.b("d") {
		clean := strings.Join(strings.Fields(string(data)), "")
		b, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(clean, "=")); err != nil {
				return fmt.Errorf("%s: bad decrypt: invalid base64 input", s.cmd)
			}
		}
		return s.write(f.s("out"), b, 0o644)
	}
	return s.write(f.s("out"), []byte(sslWrap64(base64.StdEncoding.EncodeToString(data), !f.b("A"))), 0o644)
}

func sslWrap64(e string, wrap bool) string {
	if !wrap {
		return e + "\n"
	}
	var sb strings.Builder
	for len(e) > 64 {
		sb.WriteString(e[:64] + "\n")
		e = e[64:]
	}
	if e != "" {
		sb.WriteString(e + "\n")
	}
	return sb.String()
}

func (s *ssl) rand(f *sslFlags) error {
	if len(f.pos) != 1 {
		return errors.New("rand: usage: rand [-hex|-base64] [-out file] num")
	}
	n, err := strconv.Atoi(f.pos[0])
	if err != nil || n < 0 || n > 1<<24 {
		return fmt.Errorf("rand: invalid number %q", f.pos[0])
	}
	b := sslRand(n)
	switch {
	case f.b("hex"):
		return s.write(f.s("out"), []byte(hex.EncodeToString(b)+"\n"), 0o644)
	case f.b("base64"):
		return s.write(f.s("out"), []byte(sslWrap64(base64.StdEncoding.EncodeToString(b), true)), 0o644)
	}
	return s.write(f.s("out"), b, 0o644)
}

func (s *ssl) dgst(f *sslFlags) error {
	algs := []struct {
		flag, name string
		h          func() hash.Hash
	}{{"md5", "MD5", md5.New}, {"sha1", "SHA1", sha1.New}, {"sha384", "SHA2-384", sha512.New384}, {"sha512", "SHA2-512", sha512.New}, {"sha256", "SHA2-256", sha256.New}}
	name, h := "SHA2-256", sha256.New
	for _, a := range algs {
		if f.b(a.flag) || f.s("md") == a.flag {
			name, h = a.name, a.h
		}
	}
	files := f.pos
	if len(files) == 0 {
		files = []string{"-"}
	}
	var buf bytes.Buffer
	for _, p := range files {
		data, err := s.read(p)
		if err != nil {
			return err
		}
		label := p
		if p == "-" {
			label = "stdin"
		}
		var sum []byte
		if f.b("hmac") {
			m := hmac.New(h, []byte(f.s("hmac")))
			m.Write(data)
			sum, name = m.Sum(nil), strings.TrimPrefix(name, "HMAC-")
			fmt.Fprintf(&buf, "HMAC-%s(%s)= %s\n", name, label, hex.EncodeToString(sum))
			continue
		}
		d := h()
		d.Write(data)
		fmt.Fprintf(&buf, "%s(%s)= %s\n", name, label, hex.EncodeToString(d.Sum(nil)))
	}
	return s.write(f.s("out"), buf.Bytes(), 0o644)
}

func sslCerts(data []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return out
		}
		if b.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(b.Bytes); err == nil {
				out = append(out, c)
			}
		}
	}
}

func (s *ssl) verify(f *sslFlags) error {
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	for _, p := range f.v["CAfile"] {
		d, err := s.read(p)
		if err != nil {
			return err
		}
		for _, c := range sslCerts(d) {
			roots.AddCert(c)
		}
	}
	for _, p := range f.v["untrusted"] {
		d, err := s.read(p)
		if err != nil {
			return err
		}
		for _, c := range sslCerts(d) {
			inter.AddCert(c)
		}
	}
	files := f.pos
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, p := range files {
		label := p
		if p == "-" {
			label = "stdin"
		}
		d, err := s.read(p)
		if err != nil {
			return err
		}
		cs := sslCerts(d)
		if len(cs) == 0 {
			fmt.Fprintf(s.hc.Stderr, "error %s: unable to load certificate\n", label)
			code = 2
			continue
		}
		c := cs[0]
		_, err = c.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		if err == nil {
			fmt.Fprintf(s.hc.Stdout, "%s: OK\n", label)
			continue
		}
		code = 2
		msg := err.Error()
		var inv x509.CertificateInvalidError
		switch {
		case errors.As(err, &inv) && inv.Reason == x509.Expired && time.Now().After(c.NotAfter):
			msg = "error 10 at 0 depth lookup: certificate has expired"
		case errors.As(err, &inv) && inv.Reason == x509.Expired:
			msg = "error 9 at 0 depth lookup: certificate is not yet valid"
		case errors.As(err, &inv) && inv.Reason == x509.NotAuthorizedToSign:
			msg = "error 79 at 1 depth lookup: invalid CA certificate"
		case bytes.Equal(c.RawIssuer, c.RawSubject):
			msg = "error 18 at 0 depth lookup: self-signed certificate"
		default:
			var ua x509.UnknownAuthorityError
			if errors.As(err, &ua) {
				msg = "error 20 at 0 depth lookup: unable to get local issuer certificate"
			}
		}
		fmt.Fprintf(s.hc.Stderr, "%s\n%s\nerror %s: verification failed\n", sslName(c.RawSubject), msg, label)
	}
	if code != 0 {
		return interp.ExitStatus(uint8(code))
	}
	return nil
}
