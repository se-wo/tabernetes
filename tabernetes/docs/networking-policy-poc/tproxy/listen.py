# Proof of concept for docs/networking-policy.md, see tproxy.sh.
import socket, sys
IP_TRANSPARENT = 19
s = socket.socket(); s.setsockopt(socket.SOL_IP, IP_TRANSPARENT, 1)
s.bind(("0.0.0.0", 15001)); s.listen()
c, a = s.accept()
print("accepted from", a, "original dst", c.getsockname(), flush=True)
