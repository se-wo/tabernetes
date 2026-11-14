// Small proxy components for the tests of the proxy path (web/src/http.js and
// serveProxy of web/src/component.js), from the WebAssembly text format. They
// import a small part of wasi:http/types: the variant error-code has only
// the case internal-error. jco then makes the same JavaScript for the
// imports that the test uses as for a complete proxy component.
import { parse } from "@bytecodealliance/jco";

// The behaviors of handle:
//   ok       a response with the status 200 + a global counter (PX7)
//   wait     a wait of ms milliseconds on a pollable of monotonic-clock, then ok
//   trap     unreachable (PX21)
//   none     handle returns and sets no response (PX26)
//   error    response-outparam.set with internal-error (PX27)
const BODIES = {
  ok: "",
  wait: "(local.set $p (call $sub (i64.mul (i64.const MS) (i64.const 1000000)))) (call $block (local.get $p)) (call $drop_p (local.get $p))",
  trap: "unreachable",
  none: "(call $drop_ir (local.get $req)) (return)",
  error: "(call $set (local.get $out) (i32.const 1) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)) (return)",
};

export async function proxyComponent(mode, { ms = 0, pages = 1 } = {}) {
  const body = BODIES[mode].replace("MS", String(ms));
  return parse(`(component
  (import "wasi:io/poll@0.2.3" (instance $poll
    (export "pollable" (type $pb (sub resource)))
    (export "[method]pollable.block" (func (param "self" (borrow $pb))))))
  (alias export $poll "pollable" (type $pollable))
  (import "wasi:clocks/monotonic-clock@0.2.3" (instance $clock
    (alias outer 1 $pollable (type $outer))
    (export "pollable" (type $pc (eq $outer)))
    (export "subscribe-duration" (func (param "when" u64) (result (own $pc))))))
  (import "wasi:http/types@0.2.3" (instance $http
    (export "fields" (type $f (sub resource)))
    (export "incoming-request" (type $irq (sub resource)))
    (export "outgoing-response" (type $or (sub resource)))
    (export "response-outparam" (type $rop (sub resource)))
    (type $ecv (variant (case "internal-error" (option string))))
    (export "error-code" (type $ec (eq $ecv)))
    (type $res (result (own $or) (error $ec)))
    (export "[constructor]fields" (func (result (own $f))))
    (export "[constructor]outgoing-response" (func (param "headers" (own $f)) (result (own $or))))
    (export "[method]outgoing-response.set-status-code" (func (param "self" (borrow $or)) (param "status-code" u16) (result (result))))
    (export "[static]response-outparam.set" (func (param "param" (own $rop)) (param "response" $res)))))
  (alias export $http "incoming-request" (type $ir))
  (alias export $http "response-outparam" (type $ro))

  (core module $mem (memory (export "memory") ${pages}))
  (core instance $mi (instantiate $mem))
  (alias core export $mi "memory" (core memory $m))

  (core func $block (canon lower (func $poll "[method]pollable.block")))
  (core func $sub (canon lower (func $clock "subscribe-duration")))
  (core func $fields (canon lower (func $http "[constructor]fields")))
  (core func $resp (canon lower (func $http "[constructor]outgoing-response")))
  (core func $status (canon lower (func $http "[method]outgoing-response.set-status-code")))
  (core func $set (canon lower (func $http "[static]response-outparam.set") (memory $m)))
  (core func $drop_ir (canon resource.drop $ir))
  (core func $drop_p (canon resource.drop $pollable))
  (core instance $x
    (export "block" (func $block))
    (export "sub" (func $sub))
    (export "fields" (func $fields))
    (export "resp" (func $resp))
    (export "status" (func $status))
    (export "set" (func $set))
    (export "drop-ir" (func $drop_ir))
    (export "drop-p" (func $drop_p))
    (export "memory" (memory $m)))

  (core module $main
    (import "x" "block" (func $block (param i32)))
    (import "x" "sub" (func $sub (param i64) (result i32)))
    (import "x" "fields" (func $fields (result i32)))
    (import "x" "resp" (func $resp (param i32) (result i32)))
    (import "x" "status" (func $status (param i32 i32) (result i32)))
    (import "x" "set" (func $set (param i32 i32 i32 i32 i32 i32)))
    (import "x" "drop-ir" (func $drop_ir (param i32)))
    (import "x" "drop-p" (func $drop_p (param i32)))
    (import "x" "memory" (memory 1))
    (global $count (mut i32) (i32.const 0))
    (func (export "handle") (param $req i32) (param $out i32) (local $p i32) (local $r i32)
      ${body}
      (call $drop_ir (local.get $req))
      (global.set $count (i32.add (global.get $count) (i32.const 1)))
      (local.set $r (call $resp (call $fields)))
      (drop (call $status (local.get $r) (i32.add (i32.const 200) (global.get $count))))
      (call $set (local.get $out) (i32.const 0) (local.get $r) (i32.const 0) (i32.const 0) (i32.const 0))))
  (core instance $i (instantiate $main (with "x" (instance $x))))

  (func $handle (param "request" (own $ir)) (param "response-out" (own $ro)) (canon lift (core func $i "handle")))
  (instance $h (export "handle" (func $handle)))
  (export "wasi:http/incoming-handler@0.2.3" (instance $h))
)`);
}
