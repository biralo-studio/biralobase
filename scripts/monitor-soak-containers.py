#!/usr/bin/env python3
"""Retain Docker state, process/cgroup memory and logs for disposable soaks.

Run on the Docker host. This never restarts containers or interprets progress as
a pass. Use a private output directory: logs may contain application details.
"""
import argparse
import datetime
import json
import pathlib
import subprocess
import time


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE, timeout=20)


def key_values(path):
    result = {}
    for line in path.read_text().splitlines():
        fields = line.replace(":", "").split()
        if len(fields) >= 2 and fields[1].isdigit():
            result[fields[0]] = int(fields[1])
    return result


def sample(name, output):
    container = json.loads(command("docker", "inspect", name))[0]
    state = container["State"]
    value = {
        "name": name, "image": container["Image"], "state": state,
        "restartCount": container["RestartCount"],
    }
    pid = state["Pid"]
    if pid:
        try:
            status = key_values(pathlib.Path(f"/proc/{pid}/status"))
            value["processKiB"] = {k: status[k] for k in ("VmRSS", "VmHWM", "RssAnon", "RssFile", "RssShmem") if k in status}
            group = next(line[3:] for line in pathlib.Path(f"/proc/{pid}/cgroup").read_text().splitlines() if line.startswith("0::"))
            root = pathlib.Path("/sys/fs/cgroup") / group.lstrip("/")
            value["cgroup"] = {}
            for key in ("memory.current", "memory.peak", "memory.events", "memory.stat", "cpu.stat"):
                path = root / key
                if path.exists():
                    value["cgroup"][key] = int(path.read_text()) if key in ("memory.current", "memory.peak") else key_values(path)
        except (OSError, StopIteration, ValueError) as error:
            # The process can exit between inspect and /proc reads.
            value["processSampleError"] = str(error)
    log = subprocess.run(["docker", "logs", "--timestamps", name], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=20, check=True).stdout
    temporary = output / (name + ".log.tmp")
    temporary.write_text(log)
    temporary.replace(output / (name + ".log"))
    (output / (name + ".state.json")).write_text(json.dumps(value, indent=2) + "\n")
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--hours", type=float, default=26)
    parser.add_argument("containers", nargs="+")
    args = parser.parse_args()
    if not 0 < args.hours <= 48:
        parser.error("hours must be between zero and 48")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=True)
    deadline = time.monotonic() + args.hours * 3600
    with (args.output / "containers.jsonl").open("a", buffering=1) as stream:
        while True:
            event = {"utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "containers": []}
            for name in args.containers:
                try:
                    event["containers"].append(sample(name, args.output))
                except (OSError, subprocess.SubprocessError, ValueError, KeyError) as error:
                    event["containers"].append({"name": name, "sampleError": str(error)})
            stream.write(json.dumps(event) + "\n")
            if all(c.get("state", {}).get("Status") in ("exited", "dead") for c in event["containers"]):
                return
            if time.monotonic() >= deadline:
                raise SystemExit("Monitoring deadline reached; test outcomes may still be pending")
            time.sleep(60)


if __name__ == "__main__":
    main()
