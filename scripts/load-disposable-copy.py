#!/usr/bin/env python3
"""Bounded CRUD load against a synthetic collection in a disposable copy.

The collection bb_review_items must be deployed first. Requests are never
retried: report failures alongside throughput. No existing records are changed.
"""
import argparse
import collections
import concurrent.futures
import importlib.util
import json
import pathlib
import secrets
import time

spec = importlib.util.spec_from_file_location('comparison', pathlib.Path(__file__).with_name('compare-disposable-copies.py'))
comparison = importlib.util.module_from_spec(spec)
spec.loader.exec_module(comparison)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--candidate', required=True)
    p.add_argument('--label', required=True)
    p.add_argument('--email', required=True)
    p.add_argument('--password-file', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--iterations', type=int, default=40)
    p.add_argument('--allow-synthetic-writes', action='store_true', required=True)
    args = p.parse_args()
    if not 1 <= args.iterations <= 1000:
        raise ValueError('iterations must be between 1 and 1000')
    admin = comparison.Client(args.candidate)
    admin.login(args.email, pathlib.Path(args.password_file).read_text().rstrip('\r\n'))
    path = '/api/collections/bb_review_items/records'
    status, _ = admin.request('GET', path + '?perPage=1')
    if status != 200:
        raise RuntimeError('synthetic collection is unavailable')
    prefix = 'load-' + secrets.token_hex(8)
    report = {'label': args.label, 'retryPolicy': 'none', 'rounds': []}
    def worker(number):
        client = comparison.Client(args.candidate)
        client.token = admin.token
        latencies, failures, created = [], collections.Counter(), []
        def call(method, endpoint, body=None, expected=200):
            start = time.perf_counter()
            try:
                status, result = client.request(method, endpoint, body)
            except Exception as e:
                failures['transport:' + type(e).__name__] += 1
                return None
            finally:
                latencies.append((time.perf_counter() - start) * 1000)
            if status != expected:
                failures[method + ':' + str(status)] += 1
                return None
            return result
        for i in range(args.iterations):
            payload = {'nested': [number, i, {'value': 'synthetic'}]}
            record = call('POST', path, {'key': f'{prefix}-{number}-{i}', 'payload': payload, 'value': i})
            if record is None:
                continue
            created.append(record['id'])
            endpoint = path + '/' + record['id']
            result = call('GET', endpoint)
            if result is not None and result['payload'] != payload:
                failures['incorrect JSON read'] += 1
            result = call('PATCH', endpoint, {'value': i + 1})
            if result is not None:
                result = call('GET', endpoint)
                if result is not None and result['value'] != i + 1:
                    failures['incorrect update read'] += 1
        return latencies, failures, created
    all_created = []
    try:
        for concurrency in [1, 8, 16]:
            start = time.perf_counter()
            with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                results = list(pool.map(worker, range(concurrency)))
            elapsed = time.perf_counter() - start
            latencies, failures = [], collections.Counter()
            for times, errors, ids in results:
                latencies.extend(times); failures.update(errors); all_created.extend(ids)
            latencies.sort()
            report['rounds'].append({'concurrency': concurrency, 'requests': len(latencies), 'seconds': round(elapsed, 3), 'requestsPerSecond': round(len(latencies)/elapsed, 1), 'p95Milliseconds': round(latencies[min(len(latencies)-1, int(len(latencies)*.95))], 2), 'failures': dict(failures)})
    finally:
        cleanup_failures = 0
        for rid in all_created:
            status, _ = admin.request('DELETE', path + '/' + rid)
            if status != 204:
                cleanup_failures += 1
        report['cleanupFailures'] = cleanup_failures
    report['passed'] = not cleanup_failures and all(not r['failures'] for r in report['rounds'])
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
