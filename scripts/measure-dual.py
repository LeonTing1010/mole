#!/usr/bin/env python3
"""Bounded download tests through the two fixed, owned-node keeper forwarders."""
import argparse
import concurrent.futures
import json
import pathlib
import sys
import threading
import time
import urllib.request


def state_summary():
    try:
        state = json.loads((pathlib.Path.home() / ".mole/state.json").read_text())
        return {key: state.get(key) for key in (
            "bandwidth_down_mbps", "reality_rate_mbps", "balance")}
    except (OSError, ValueError):
        return {}


class OverflowWatch:
    def __init__(self):
        self.path = pathlib.Path.home() / ".mole/mole.log"
        try:
            info = self.path.stat()
            self.inode, self.offset = info.st_ino, info.st_size
        except OSError:
            self.inode, self.offset = None, 0
        self.tail = b""
        self.count = 0

    def poll(self):
        try:
            info = self.path.stat()
            if info.st_ino != self.inode or info.st_size < self.offset:
                self.inode, self.offset, self.tail = info.st_ino, 0, b""
            with self.path.open("rb") as stream:
                stream.seek(self.offset)
                data = stream.read()
                self.offset = stream.tell()
            marker = b"write: no buffer space available"
            combined = self.tail + data
            self.count += combined.count(marker)
            self.tail = combined[-(len(marker) - 1):]
        except OSError:
            pass
        return self.count


def run_phase(name, ports, seconds, budget, workers, watch=None,
              request_bytes=8 << 20, opener_factory=None):
    lock, stop, start = threading.Lock(), threading.Event(), threading.Event()
    stats = {port: {"bytes": 0, "started": 0, "completed": 0, "aborted": 0, "timed_out": 0, "failed": 0, "errors": []} for port in ports}
    remaining = budget
    before = watch.poll() if watch else 0
    began = time.monotonic()
    deadline = began + seconds

    def worker(port):
        nonlocal remaining
        opener = (opener_factory() if opener_factory else
                  urllib.request.build_opener(urllib.request.ProxyHandler({})))
        start.wait()
        while not stop.is_set() and time.monotonic() < deadline:
            with lock:
                size = min(request_bytes, remaining)
                remaining -= size  # Reservation is not delivery.
                if size:
                    stats[port]["started"] += 1
            if not size:
                return
            received = 0
            try:
                url = f"http://127.0.0.1:{port}/bulk?bytes={size}"
                with opener.open(url, timeout=8) as response:
                    if response.status != 200 or int(response.headers.get("Content-Length", 0)) != size:
                        raise ValueError("unexpected keeper payload/status")
                    received = 0
                    while not stop.is_set() and time.monotonic() < deadline:
                        chunk = response.read(min(64 << 10, size - received))
                        if not chunk:
                            break
                        received += len(chunk)
                        with lock:
                            stats[port]["bytes"] += len(chunk)
                        if received == size:
                            with lock:
                                stats[port]["completed"] += 1
                            break
                    if received != size and not stop.is_set() and time.monotonic() < deadline:
                        raise ValueError(f"incomplete payload {received}/{size}")
            except Exception as error:
                with lock:
                    stats[port]["failed"] += 1
                    stats[port]["errors"].append(str(error)[:240])
                return
            else:
                if received != size:
                    with lock:
                        key = "timed_out" if time.monotonic() >= deadline else "aborted"
                        stats[port][key] += 1
                    return

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(ports) * workers) as pool:
        jobs = [pool.submit(worker, port) for port in ports for _ in range(workers)]
        began = time.monotonic()
        deadline = began + seconds
        start.set()
        while not all(job.done() for job in jobs):
            if watch and watch.poll() > before:
                stop.set()
            if time.monotonic() >= deadline:
                stop.set()
            stop.wait(0.1) if not stop.is_set() else time.sleep(0.1)
        for job in jobs:
            job.result()
    elapsed = time.monotonic() - began
    for value in stats.values():
        value["mbps"] = value["bytes"] * 8 / elapsed / 1e6
    delivered = sum(value["bytes"] for value in stats.values())
    enobufs = (watch.poll() - before) if watch else 0
    errors = sum(len(value["errors"]) for value in stats.values())
    complete = delivered == budget and not errors and not enobufs
    if enobufs:
        reason = "enobufs"
    elif errors:
        reason = "request_error"
    elif complete:
        reason = "budget_delivered"
    else:
        reason = "deadline"
    return {"phase": name, "elapsed_seconds": elapsed, "legs": stats,
            "total_connections": len(ports) * workers,
            "connections_per_leg": workers,
            "aggregate_mbps": sum(value["mbps"] for value in stats.values()),
            "complete": complete, "stop_reason": reason,
            "budget_bytes": budget, "delivered_bytes": delivered,
            "budget_reserved_bytes": budget - remaining,
            "budget_reserved_exhausted": remaining == 0,
            "budget_exhausted": complete,
            "new_enobufs_lines": enobufs}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=int, default=15, choices=range(5, 61), metavar="5..60")
    parser.add_argument("--max-mib", type=int, default=256, choices=range(32, 1025), metavar="32..1024")
    parser.add_argument("--connections", type=int, default=8, choices=(2, 4, 8, 16),
                        help="total concurrent connections in EVERY phase (default: 8)")
    parser.add_argument("--output", default="/tmp/mole-dual-measure.json")
    args = parser.parse_args()
    print(f"单路与双路下载测试，每阶段共 {args.connections} 条连接，最多 {args.max_mib} MiB，遇到新的 ENOBUFS 自动停止。", flush=True)
    watch = OverflowWatch()
    report = {"started_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
              "before": state_summary(), "phases": [],
              "note": "Actual received payload over wall time; owned-node download only. Not VPS port capacity or local upload speed."}
    phases = [("Reality", [8098]), ("Hysteria2", [8099]), ("Both", [8098, 8099])]
    for name, ports in phases:
        # Keep limits alongside each measurement: the live scheduler can retune.
        before = state_summary()
        phase = run_phase(name, ports, args.seconds, args.max_mib << 20,
                          args.connections // len(ports), watch)
        phase["state_before"], phase["state_after"] = before, state_summary()
        report["phases"].append(phase)
        errors = sum(len(v["errors"]) for v in phase["legs"].values())
        result = f"合计 {phase['aggregate_mbps']:.1f} Mbps" if phase["complete"] else f"未完成（{phase['stop_reason']}），已收数据速率 {phase['aggregate_mbps']:.1f} Mbps，不作为容量结果"
        print(f"{name}: {result} / {phase['elapsed_seconds']:.1f}s; "
              f"收齐预算 {phase['complete']}; 错误 {errors}; "
              f"新增 ENOBUFS {phase['new_enobufs_lines']}", flush=True)
        if not phase["complete"]:
            print("测试已停止；报告保留失败信息。", flush=True)
            break
        if name != "Both":
            time.sleep(3)
    report["after"] = state_summary()
    pathlib.Path(args.output).write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(f"报告：{args.output}。两路结果已按同一时间窗计算，不把单独测速相加。")
    return 0 if len(report["phases"]) == 3 and not any(
        not p["complete"]
        for p in report["phases"]) else 2


if __name__ == "__main__":
    sys.exit(main())
