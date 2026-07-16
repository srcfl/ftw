from __future__ import annotations

import argparse
import json
import os
import socket


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", default=os.environ.get("FTW_OPTIMIZER_SOCKET", "/run/ftw-optimizer/optimizer.sock"))
    args = parser.parse_args()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(4)
        client.connect(args.socket)
        client.sendall(b'{"type":"handshake","protocol_version":1}\n')
        response = json.loads(client.makefile("r", encoding="utf-8").readline())
    if response.get("protocol_version") != 1:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
