"""Generates vectors.json for the cachesalt package.

This is a deliberately independent implementation of the same construction:

    Sum(tag, f1, ..., fn) = SHA-256(tag || lp(f1) || ... || lp(fn))
    lp(x)                 = uint32_be(len(x)) || x

    salt = urlsafe_b64_nopad(Sum("tinfoil/cache-salt/v1", tenant_id))                     # no secret
    salt = urlsafe_b64_nopad(Sum("tinfoil/cache-salt/v1", tenant_id, user_cache_secret))  # with secret

It exists so the Go package is tested against outputs it did not produce.
Never port logic from the Go code into this file: if the two disagree, the
bug hunt starts from the spec above, not from making them match.

Run from this directory:  python3 gen_vectors.py
"""

import base64
import hashlib
import json
import os
import struct

SALT_TAG = "tinfoil/cache-salt/v1"
TEST_TAG = "tinfoil/test/v1"


def lp(x: bytes) -> bytes:
    return struct.pack(">I", len(x)) + x


def sum_(tag: str, fields: list[str]) -> bytes:
    buf = tag.encode()
    for f in fields:
        buf += lp(f.encode())
    return hashlib.sha256(buf).digest()


def salt(tenant: str, secret: str) -> str:
    fields = [tenant] if secret == "" else [tenant, secret]
    return base64.urlsafe_b64encode(sum_(SALT_TAG, fields)).decode().rstrip("=")


derive_cases = [
    # The boundary-shift pair: without lp() framing these two concatenate to
    # identical bytes and MUST NOT collide.
    ("boundary-shift victim: tenant salt of org_12345", "org_12345", ""),
    ("boundary-shift attacker: user salt of org_1 + 2345", "org_1", "2345"),
    ("tenant salt of the attacker tenant itself", "org_1", ""),
    ("tenant salt vs user salt, same tenant", "tenant-a", ""),
    ("user salt subdivides tenant: secret-1", "tenant-a", "secret-1"),
    ("user salt subdivides tenant: secret-2", "tenant-a", "secret-2"),
    ("jwt-sub-shaped tenant id", "user_2NxTybBAqywNWUxya8FOYlgIRRK", ""),
    ("raw-api-key-shaped tenant id", "tk_live_9f8e7d6c5b4a3210", ""),
    ("unicode tenant and secret", "org_ü", "秘密"),
    ("secret containing lp()-shaped bytes", "tenant-a", "a\x00\x00\x00\x01b"),
    ("secret equals another tenant's full id", "org_1", "org_12345"),
    ("smuggled frame: tenant id embeds the lp() encoding of the attacker pair", "org_1\x00\x00\x00\x042345", ""),
    ("multi-byte length: 300-byte secret exercises the second length byte", "tenant-a", "s" * 300),
    ("long tenant id", "t" * 128, ""),
]

sum_cases = [
    (TEST_TAG, []),
    (TEST_TAG, [""]),
    (TEST_TAG, ["a", "bc"]),
    (TEST_TAG, ["ab", "c"]),
    (TEST_TAG, ["abc"]),
    (TEST_TAG, ["abc", ""]),
    (TEST_TAG, ["a\x00\x00\x00\x01b"]),
    (SALT_TAG, ["org_12345"]),
]

out = {
    "comment": "Pinned outputs of the domain-tagged, length-prefixed SHA-256 construction (see gen_vectors.py in this directory, an implementation independent of the Go package). Never regenerate this file from the Go code.",
    "derive": [
        {"note": n, "tenant_id": t, "user_cache_secret": s,
         "salt": salt(t, s), "mode": "tenant" if s == "" else "user"}
        for (n, t, s) in derive_cases
    ],
    "sum": [
        {"tag": tag, "fields": fields, "sha256_hex": sum_(tag, fields).hex()}
        for (tag, fields) in sum_cases
    ],
}

path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "vectors.json")
with open(path, "w") as f:
    json.dump(out, f, indent=2)
    f.write("\n")

# sanity: the boundary-shift pair really differs, and every salt is 43 chars
assert salt("org_1", "2345") != salt("org_12345", "")
assert all(len(salt(t, s)) == 43 for (_, t, s) in derive_cases)
print("wrote", path)
