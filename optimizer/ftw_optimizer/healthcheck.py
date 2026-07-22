from __future__ import annotations

import argparse
import json
import os
import socket


REQUIRED_NAME = "ftw-optimizer"
REQUIRED_PROTOCOL_VERSION = 1
REQUIRED_FEATURE = "champion"


def validate_handshake(response: object) -> None:
    if not isinstance(response, dict):
        raise ValueError("optimizer handshake must be an object")
    if response.get("name") != REQUIRED_NAME:
        raise ValueError("optimizer handshake has the wrong name")
    if response.get("protocol_version") != REQUIRED_PROTOCOL_VERSION:
        raise ValueError("optimizer handshake has an incompatible protocol version")
    features = response.get("features")
    if not isinstance(features, list) or REQUIRED_FEATURE not in features:
        raise ValueError("optimizer handshake is missing the champion feature")
    version = response.get("version")
    if not isinstance(version, str) or not version.strip():
        raise ValueError("optimizer handshake is missing its runtime version")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", default=os.environ.get("FTW_OPTIMIZER_SOCKET", "/run/ftw-optimizer/optimizer.sock"))
    args = parser.parse_args()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(4)
        client.connect(args.socket)
        client.sendall(b'{"type":"handshake","protocol_version":1}\n')
        response = json.loads(client.makefile("r", encoding="utf-8").readline())
    try:
        validate_handshake(response)
    except ValueError as exc:
        raise SystemExit(str(exc)) from exc


if __name__ == "__main__":
    main()
