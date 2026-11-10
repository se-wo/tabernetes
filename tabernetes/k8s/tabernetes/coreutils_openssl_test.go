package main

import (
	"strings"
	"testing"
)

func TestOpenSSLCSRFlow(t *testing.T) {
	out, code := sh(t, `
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out jane.key
openssl req -new -key jane.key -subj "/CN=jane/O=dev" -out jane.csr
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout ca.key -subj /CN=ca -days 365 -out ca.crt
openssl x509 -req -in jane.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -out jane.crt
openssl x509 -in jane.crt -noout -text -subject -issuer
openssl verify -CAfile ca.crt jane.crt
openssl req -in jane.csr -noout -verify
`)
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	for _, w := range []string{"Subject: CN = jane, O = dev", "Issuer: CN = ca", "Version: 3 (0x2)", "ecdsa-with-SHA256",
		"subject=CN = jane, O = dev", "issuer=CN = ca", "jane.crt: OK", "verify OK", "NIST CURVE: P-256"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestOpenSSLRSA(t *testing.T) {
	out, code := sh(t, `
openssl genrsa -out jane.key 2048
head -1 jane.key
openssl req -new -key jane.key -subj "/CN=jane/O=dev" -out jane.csr
openssl req -in jane.csr -noout -text
openssl rsa -in jane.key -pubout
`)
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	for _, w := range []string{"-----BEGIN PRIVATE KEY-----", "Public-Key: (2048 bit)", "Exponent: 65537 (0x10001)", "Subject: CN = jane, O = dev",
		"sha256WithRSAEncryption", "Requested Extensions:", "BEGIN PUBLIC KEY"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestOpenSSLMisc(t *testing.T) {
	for _, c := range []struct {
		script, want string
		code         int
	}{
		{`printf hello > f; openssl base64 -in f`, "aGVsbG8=\n", 0},
		{`printf hello > f; openssl base64 -in f -out g; openssl enc -base64 -d -in g`, "hello", 0},
		{`printf hello > f; openssl dgst -sha256 f`, "SHA2-256(f)= 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824\n", 0},
		{`openssl rand -hex 8 | wc -c`, "", 0},
		{`openssl version`, "OpenSSL 3.0.13", 0},
		{`openssl s_client`, "not supported in the browser", 1},
		{`openssl foo`, `Invalid command 'foo'; type "help" for a list.`, 1},
		{`openssl req -new -key k`, "no -subj given", 1},
		{`openssl req -x509 -newkey ed25519 -keyout k -subj "/CN=a/O=x/O=y\/z/C=DE" -out c; openssl x509 -in c -noout -subject -text`, "subject=CN = a, O = x, O = y/z, C = DE", 0},
		{`openssl req -x509 -newkey ec -keyout k -subj /CN=a -addext "subjectAltName=DNS:a.b,IP:1.2.3.4" -out c; openssl x509 -in c -noout -ext x -text`, "DNS:a.b, IP Address:1.2.3.4", 0},
		{`openssl req -x509 -newkey ec -keyout k -subj /CN=a -out c; openssl req -x509 -newkey ec -keyout k2 -subj /CN=b -out c2; openssl verify -CAfile c c2`, "verification failed", 2},
	} {
		out, code := sh(t, c.script)
		if code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: code %d out %q, want %d %q", c.script, code, out, c.code, c.want)
		}
	}
}
