"""Sample the resident memory of a running, idle Cove panel process (database excluded)."""
import argparse
import json
from pathlib import Path
import platform
import statistics
import subprocess
import time


def resident_bytes(pid):
    if platform.system() == 'Windows':
        return int(subprocess.check_output(
            ['powershell', '-NoProfile', '-NonInteractive', '-Command',
             f'(Get-Process -Id {pid} -ErrorAction Stop).WorkingSet64'], text=True).strip())
    status = Path(f'/proc/{pid}/status').read_text()
    for line in status.splitlines():
        if line.startswith('VmRSS:'):
            return int(line.split()[1]) * 1024
    raise RuntimeError('Process resident memory is unavailable')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('pid', type=int)
    parser.add_argument('--samples', type=int, default=60)
    parser.add_argument('--interval', type=float, default=1)
    parser.add_argument('--warmup', type=float, default=30)
    args = parser.parse_args()
    if args.pid <= 0 or not 2 <= args.samples <= 3600 or not 0.1 <= args.interval <= 60 or not 0 <= args.warmup <= 3600:
        parser.error('Use a positive PID, 2–3600 samples, interval 0.1–60s, warmup 0–3600s')
    time.sleep(args.warmup)
    samples = []
    started = time.monotonic()
    for i in range(args.samples):
        samples.append(resident_bytes(args.pid))
        if i + 1 < args.samples:
            time.sleep(args.interval)
    print(json.dumps({'environment': platform.platform(), 'architecture': platform.machine(),
                      'metric': 'process resident working set; excludes PostgreSQL, proxy and workers',
                      'samples': len(samples), 'elapsed_seconds': round(time.monotonic() - started, 2),
                      'warmup_seconds': args.warmup, 'min_bytes': min(samples),
                      'median_bytes': statistics.median(samples), 'max_bytes': max(samples)}, indent=2))


if __name__ == '__main__':
    main()
