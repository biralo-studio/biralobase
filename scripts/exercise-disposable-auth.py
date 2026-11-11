#!/usr/bin/env python3
"""Exercise user rules, function delegation and OAuth/MCP on DISPOSABLE copies.

This intentionally resets one copied verified user's password on both copies.
It uses loopback endpoints only and never prints passwords, tokens or records.
"""
import argparse
import base64
import hashlib
import importlib.util
import json
import pathlib
import re
import secrets
import urllib.parse

spec = importlib.util.spec_from_file_location("comparison", pathlib.Path(__file__).with_name("compare-disposable-copies.py"))
comparison = importlib.util.module_from_spec(spec)
spec.loader.exec_module(comparison)
Client = comparison.Client


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reference", default="http://127.0.0.1:18091")
    parser.add_argument("--candidate", default="http://127.0.0.1:18090")
    parser.add_argument("--email", required=True)
    parser.add_argument("--password-file", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--allow-user-credential-changes", action="store_true", required=True)
    parser.add_argument("--origin", default="https://biralobase-review.invalid")
    args = parser.parse_args()
    admins = [Client(args.reference), Client(args.candidate)]
    password = pathlib.Path(args.password_file).read_text().rstrip("\r\n")
    for client in admins:
        client.login(args.email, password)
    collections = [c for c in admins[0].all("/api/collections") if not c.get("system")]
    user = next(r for r in admins[0].all("/api/collections/users/records") if r.get("verified") and r.get("email"))
    audits = next(c for c in collections if c["name"] == "audits")
    user_path = "/api/collections/users/records/" + user["id"]
    temporary = secrets.token_urlsafe(32)
    report = {"userCollectionChecks": 0, "functionDelegationChecks": 0, "oauthMCP": False, "revocation": False}
    try:
        users = [Client(args.reference), Client(args.candidate)]
        for admin, client in zip(admins, users):
            status, _ = admin.request("PATCH", user_path, {"password": temporary, "passwordConfirm": temporary})
            require(status == 200, "copied-user password setup failed")
            status, result = client.request("POST", "/api/collections/users/auth-with-password", {"identity": user["email"], "password": temporary})
            require(status == 200, "copied-user login failed")
            client.token = result["token"]
        for collection in collections:
            path = f'/api/collections/{collection["id"]}/records?perPage=100&sort=id'
            results = [c.request("GET", path) for c in users]
            # Both copied passwords were changed at different wall-clock times.
            if collection["type"] == "auth":
                for _, result in results:
                    for row in (result or {}).get("items", []):
                        row.pop("updated", None)
            require(results[0] == results[1], "user permission/results differ for " + collection["name"])
            report["userCollectionChecks"] += 1
            forwarded = users[1].request("POST", "/api/functions/records", {"path": path})
            if collection["type"] == "auth":
                for row in (forwarded[1] or {}).get("items", []):
                    row.pop("updated", None)
            require(forwarded == results[1], "function delegation changed user permissions for " + collection["name"])
            report["functionDelegationChecks"] += 1
        client = users[1]
        status, registered = client.request("POST", "/oauth/register", {"client_name": "Disposable qualification client", "redirect_uris": ["http://127.0.0.1:18999/callback"], "token_endpoint_auth_method": "none"})
        require(status == 201, "OAuth client registration failed")
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
        scope = f'bb:{audits["id"]}:list'
        query = {"client_id": registered["client_id"], "redirect_uri": "http://127.0.0.1:18999/callback", "response_type": "code", "scope": scope + " offline_access", "state": secrets.token_urlsafe(16), "resource": args.origin + "/mcp", "code_challenge_method": "S256", "code_challenge": challenge}
        status, html = client.request("GET", "/oauth/authorize?" + urllib.parse.urlencode(query), raw=True)
        require(status == 200, "OAuth authorize failed")
        consent_id = json.loads(re.search(rb'const id=("[^"]+")', html).group(1))
        cookie = client.last_headers.get("Set-Cookie").split(";", 1)[0]
        status, consent = client.request("POST", "/oauth/consent", {"id": consent_id, "allow": True}, headers={"Origin": args.origin, "Cookie": cookie})
        require(status == 200, "OAuth consent failed")
        redirect = urllib.parse.parse_qs(urllib.parse.urlsplit(consent["redirect"]).query)
        require(redirect.get("state") == [query["state"]], "OAuth state was not preserved")
        form = {"grant_type": "authorization_code", "code": redirect["code"][0], "code_verifier": verifier, "client_id": registered["client_id"], "redirect_uri": query["redirect_uri"], "resource": query["resource"]}
        status, tokens = client.request("POST", "/oauth/token", urllib.parse.urlencode(form).encode(), headers={"Content-Type": "application/x-www-form-urlencoded"})
        require(status == 200 and tokens.get("refresh_token"), "OAuth code exchange failed")
        bearer = {"Authorization": "Bearer " + tokens["access_token"], "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-06-18"}
        def rpc(method, params=None):
            return client.request("POST", "/mcp", {"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}, headers=bearer)
        status, result = rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "qualification", "version": "1"}})
        require(status == 200 and "result" in result, "MCP initialization failed")
        status, result = rpc("tools/list")
        require(status == 200 and [tool["name"] for tool in result["result"]["tools"]] == ["audits_list"], "MCP tools escaped granted scopes")
        status, result = rpc("tools/call", {"name": "audits_list", "arguments": {"perPage": 100}})
        require(status == 200 and not result["result"].get("isError"), "MCP tool call failed")
        listing = json.loads(result["result"]["content"][0]["text"])
        status, allowed = client.request("GET", f'/api/collections/{audits["id"]}/records?perPage=100&page=1')
        require(status == 200 and {r["id"] for r in listing["items"]} == {r["id"] for r in allowed["items"]}, "MCP record permissions differ from REST")
        require(all(set(r) <= {"id", "finalized"} for r in listing["items"]), "MCP exposed undeclared fields")
        status, result = rpc("tools/call", {"name": "audits_get", "arguments": {"id": "not-exposed"}})
        require(status >= 400 or "error" in result or result.get("result", {}).get("isError"), "MCP accepted an ungranted tool")
        report["oauthMCP"] = True
        rotated = secrets.token_urlsafe(32)
        status, _ = admins[1].request("PATCH", user_path, {"password": rotated, "passwordConfirm": rotated})
        require(status == 200, "copied-user credential rotation failed")
        status, _ = client.request("POST", "/mcp", {}, raw=True, headers=bearer)
        require(status == 401, "MCP grant survived credential rotation")
        report["revocation"] = True
    finally:
        for admin in admins:
            disabled = secrets.token_urlsafe(48)
            status, _ = admin.request("PATCH", user_path, {"password": disabled, "passwordConfirm": disabled})
            require(status == 200, "could not revoke disposable user credentials")
    report["passed"] = True
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
