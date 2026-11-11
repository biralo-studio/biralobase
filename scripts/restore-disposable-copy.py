#!/usr/bin/env python3
"""Backup/restore drill on a disposable Konnex copy, with record/file verification.

Requires explicit mutation opt-in and a loopback endpoint. No customer records,
credentials, or file contents are persisted by this script.
"""
import argparse
import hashlib
import importlib.util
import json
import pathlib
import time
import urllib.parse
import urllib.error
import http.client

spec = importlib.util.spec_from_file_location('comparison', pathlib.Path(__file__).with_name('compare-disposable-copies.py'))
comparison = importlib.util.module_from_spec(spec)
spec.loader.exec_module(comparison)


def snapshot(client):
    digest = hashlib.sha256()
    record_count = file_count = 0
    status, result = client.request('POST', '/api/files/token')
    if status != 200:
        raise RuntimeError('file token failed')
    token = result['token']
    collections = [c for c in client.all('/api/collections') if not c.get('system')]
    for c in collections:
        digest.update(json.dumps(comparison.canonical_collection(c), sort_keys=True).encode())
        rows = client.all('/api/collections/' + c['id'] + '/records')
        digest.update(json.dumps(rows, sort_keys=True).encode())
        record_count += len(rows)
        for field in c['fields']:
            if field['type'] != 'file':
                continue
            for row in rows:
                names = row.get(field['name']) or []
                if isinstance(names, str):
                    names = [names]
                for name in names:
                    path = '/api/files/' + '/'.join(urllib.parse.quote(v, safe='') for v in [c['id'], row['id'], name])
                    status, content = client.request('GET', path + '?' + urllib.parse.urlencode({'token': token}), raw=True)
                    if status != 200:
                        raise RuntimeError('file download failed')
                    digest.update(hashlib.sha256(content).digest())
                    file_count += 1
    return {'digest': digest.hexdigest(), 'collections': len(collections), 'records': record_count, 'files': file_count}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--candidate', default='http://127.0.0.1:18090')
    p.add_argument('--email', required=True)
    p.add_argument('--password-file', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--allow-restore-and-record-changes', action='store_true', required=True)
    args = p.parse_args()
    client = comparison.Client(args.candidate)
    client.login(args.email, pathlib.Path(args.password_file).read_text().rstrip('\r\n'))
    before = snapshot(client)
    row = client.all('/api/collections/audits/records')[0]
    path = '/api/collections/audits/records/' + row['id']
    original = row['finalized']
    name = 'qualification-restore-' + str(time.time_ns()) + '.zip'
    status, _ = client.request('POST', '/api/backups', {'name': name})
    if status != 204:
        raise RuntimeError('backup creation failed: HTTP ' + str(status))
    status, changed = client.request('PATCH', path, {'finalized': not original})
    if status != 200 or changed['finalized'] == original:
        raise RuntimeError('disposable mutation failed')
    status, _ = client.request('POST', '/api/backups/' + name + '/restore')
    if status != 204:
        raise RuntimeError('restore request failed')
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            status, restored = client.request('GET', path)
            ready, _ = client.request('GET', '/api/biralobase/ready')
            if ready == 200 and status == 200 and restored['finalized'] == original:
                break
        except (urllib.error.URLError, OSError, http.client.HTTPException):
            pass
        time.sleep(1)
    else:
        raise RuntimeError('restore did not recover the original record within two minutes')
    after = snapshot(client)
    if before != after:
        raise RuntimeError('restored schema, records or files differ from the backup baseline')
    report = {'passed': True, 'backup': name, **{k: v for k, v in after.items() if k != 'digest'}}
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
