#!/usr/bin/env python3
"""Compare two private PocketBase-compatible copies without logging record data.

Both endpoints must be loopback addresses. This script reads application records
and downloads their files; it never sends requests to the source production app.
Credentials are read from a file and tokens stay in memory. Run on the test host.
"""
import argparse
import hashlib
import json
import pathlib
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Client:
    def __init__(self, base):
        parsed = urllib.parse.urlsplit(base)
        if parsed.scheme != "http" or parsed.hostname not in {"127.0.0.1", "::1", "localhost"}:
            raise ValueError("only explicit HTTP loopback test endpoints are allowed")
        self.base = base.rstrip("/")
        self.token = ""
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, path, body=None, raw=False, headers=None):
        if not path.startswith(("/api/", "/oauth/", "/mcp")):
            raise ValueError("only local API paths are allowed")
        h = {"Content-Type": "application/json"}
        if self.token:
            h["Authorization"] = self.token
        h.update(headers or {})
        data = body if isinstance(body, bytes) else None if body is None else json.dumps(body).encode()
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            response = self.opener.open(r, timeout=30)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            self.last_headers = response.headers
            result = response.read()
            return response.status, result if raw else json.loads(result or b"null")

    def login(self, email, password):
        status, result = self.request("POST", "/api/collections/_superusers/auth-with-password", {"identity": email, "password": password})
        if status != 200:
            raise RuntimeError(f"test administrator login failed (HTTP {status})")
        self.token = result["token"]

    def all(self, path):
        records = []
        for page in range(1, 10001):
            status, result = self.request("GET", f"{path}?page={page}&perPage=200&sort=id")
            if status != 200:
                raise RuntimeError(f"listing failed (HTTP {status})")
            records.extend(result["items"])
            if page >= result["totalPages"]:
                return records
        raise RuntimeError("pagination limit reached")


def canonical_collection(c):
    c = json.loads(json.dumps(c))
    c.pop("created", None)
    c.pop("updated", None)
    if c.get("type") == "view":
        for field in c.get("fields", []):
            field.pop("id", None)  # upstream regenerates random alias IDs
    return c


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reference", default="http://127.0.0.1:18091")
    parser.add_argument("--candidate", default="http://127.0.0.1:18090")
    parser.add_argument("--email", required=True)
    parser.add_argument("--password-file", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    clients = [Client(args.reference), Client(args.candidate)]
    password = pathlib.Path(args.password_file).read_text().rstrip("\r\n")
    for client in clients:
        client.login(args.email, password)
    collections = [{c["id"]: c for c in client.all("/api/collections") if not c.get("system")} for client in clients]
    if set(collections[0]) != set(collections[1]):
        raise RuntimeError("application collection inventories differ")
    report = {"collections": [], "recordsCompared": 0, "filesCompared": 0, "differences": []}
    file_tokens = []
    for client in clients:
        status, result = client.request("POST", "/api/files/token")
        if status != 200:
            raise RuntimeError(f"file token failed (HTTP {status})")
        file_tokens.append(result["token"])
    for cid, collection in collections[0].items():
        name = collection["name"]
        if canonical_collection(collection) != canonical_collection(collections[1][cid]):
            report["differences"].append({"collection": name, "kind": "schema"})
        records = [client.all(f"/api/collections/{cid}/records") for client in clients]
        if records[0] != records[1]:
            left, right = [{r["id"]: r for r in rows} for rows in records]
            fields = set()
            for rid in set(left) & set(right):
                fields.update(k for k in set(left[rid]) | set(right[rid]) if left[rid].get(k) != right[rid].get(k))
            report["differences"].append({"collection": name, "kind": "records", "counts": [len(r) for r in records], "fields": sorted(fields)})
        report["collections"].append({"name": name, "type": collection["type"], "records": len(records[0])})
        report["recordsCompared"] += len(records[0])
        for field in collection.get("fields", []):
            if field["type"] != "file":
                continue
            for record in records[0]:
                values = record.get(field["name"]) or []
                if isinstance(values, str):
                    values = [values]
                for filename in values:
                    path = "/api/files/" + "/".join(urllib.parse.quote(v, safe="") for v in [cid, record["id"], filename])
                    results = [client.request("GET", path + "?" + urllib.parse.urlencode({"token": token}), raw=True) for client, token in zip(clients, file_tokens)]
                    if any(status != 200 for status, _ in results) or hashlib.sha256(results[0][1]).digest() != hashlib.sha256(results[1][1]).digest():
                        report["differences"].append({"collection": name, "kind": "file", "statuses": [r[0] for r in results]})
                    report["filesCompared"] += 1
    report["passed"] = not report["differences"]
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k: v for k, v in report.items() if k != "collections"}))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
