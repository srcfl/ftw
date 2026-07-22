from __future__ import annotations

import tomllib
from pathlib import Path

import pytest

from ftw_optimizer.healthcheck import validate_handshake
from ftw_optimizer.release_version import (
    LAST_SHARED_RELEASE,
    validate_independent_release_base,
)


def test_package_version_stays_above_the_last_shared_release() -> None:
    pyproject = Path(__file__).parents[1] / "pyproject.toml"
    with pyproject.open("rb") as source:
        version = tomllib.load(source)["project"]["version"]
    assert validate_independent_release_base(version) > LAST_SHARED_RELEASE


@pytest.mark.parametrize("version", ["0.1.0", "1.3.1", "1.3.2-beta.1", "v1.3.2"])
def test_independent_release_base_rejects_resets_and_non_base_versions(version: str) -> None:
    with pytest.raises(ValueError):
        validate_independent_release_base(version)


def test_healthcheck_accepts_the_core_handshake_contract() -> None:
    validate_handshake(
        {
            "name": "ftw-optimizer",
            "version": "v1.3.2-beta.1",
            "protocol_version": 1,
            "features": ["champion", "recourse", "multistage"],
        }
    )


@pytest.mark.parametrize(
    "field,value",
    [
        ("name", "other-optimizer"),
        ("version", ""),
        ("protocol_version", 2),
        ("features", ["recourse", "multistage"]),
    ],
)
def test_healthcheck_rejects_handshakes_core_cannot_use(field: str, value: object) -> None:
    response = {
        "name": "ftw-optimizer",
        "version": "v1.3.2-beta.1",
        "protocol_version": 1,
        "features": ["champion", "recourse", "multistage"],
    }
    response[field] = value
    with pytest.raises(ValueError):
        validate_handshake(response)
