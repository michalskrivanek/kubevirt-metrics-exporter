#!/usr/bin/env python3
"""Export KubeVirt Metrics Exporter time series from Prometheus or Thanos."""

import argparse
import datetime as dt
import gzip
import json
import os
import re
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


METRIC_NAME_MATCHER = (
    "kme_.*|kubevirt_vmi_(storage|kvm)_.*|"
    "container_memory_(active_anon|inactive_anon|anon_thp|shmem_thp|file_thp)_bytes|"
    "node_ksmd_general_profit_bytes"
)
DURATION_RE = re.compile(r"^(\d+)([smhdw])$")
DURATION_UNITS = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}


def parse_args():
    parser = argparse.ArgumentParser(
        description=(
            "Export repository-defined KME metrics through the Prometheus range-query API. "
            "The default range is the 24 hours ending now."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""examples:
  %(prog)s
  %(prog)s --server https://prometheus.example
  %(prog)s --vmi-namespace my-project --vmi-name my-vm
  %(prog)s --output-format openmetrics --output kme-metrics.om
  %(prog)s --output - --gzip > kme-metrics.json.gz""",
    )
    parser.add_argument("-s", "--server", default=os.getenv("PROMETHEUS_URL"),
                        help="Prometheus or Thanos base URL; disables port-forward")
    parser.add_argument("-d", "--duration", default="24h",
                        help="lookback when --start is omitted (default: 24h; s, m, h, d, or w)")
    parser.add_argument("--start", help="inclusive range start in RFC 3339")
    parser.add_argument("--end", default="now", help="inclusive range end in RFC 3339 (default: now)")
    parser.add_argument("--step", default="30s", help="Prometheus query resolution (default: 30s)")
    parser.add_argument("-o", "--output", help="destination file, or - for standard output")
    parser.add_argument("--output-format", choices=("json", "openmetrics"), default="json",
                        help="output format (default: json)")
    parser.add_argument("-z", "--gzip", action="store_true", help="gzip-compress output")
    parser.add_argument("--port-forward", nargs="?", const="", metavar="RESOURCE",
                        help="port-forward a resource; without RESOURCE discovers svc/thanos-querier")
    parser.add_argument("--no-port-forward", action="store_true", help="do not port-forward")
    parser.add_argument("--port-forward-namespace", "--namespace", default="openshift-monitoring",
                        help="namespace for service discovery/port-forward (default: openshift-monitoring)")
    parser.add_argument("--vmi-namespace", help="only series with this namespace label")
    parser.add_argument("--vmi-name", help="only series with this VMI name label")
    parser.add_argument("--local-port", type=valid_port, default=9091, help="local port (default: 9091)")
    parser.add_argument("--remote-port", type=valid_port, help="remote port (default: discovered service port)")
    parser.add_argument("--port-forward-scheme", choices=("https", "http"), default="https",
                        help="scheme of port-forwarded service (default: https)")
    parser.add_argument("--client", choices=("oc", "kubectl"), help="port-forward client (default: auto)")
    parser.add_argument("--bearer-token", default=os.getenv("PROMETHEUS_BEARER_TOKEN"),
                        help="Bearer token (or PROMETHEUS_BEARER_TOKEN)")
    parser.add_argument("--ca-file", help="CA certificate for the server")
    parser.add_argument("-k", "--insecure", action="store_true", help="skip TLS certificate verification")
    return parser.parse_args()


def valid_port(value):
    port = int(value)
    if not 1 <= port <= 65535:
        raise argparse.ArgumentTypeError("must be between 1 and 65535")
    return port


def parse_time(value):
    if value == "now":
        return dt.datetime.now(dt.timezone.utc)
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise ValueError(f"invalid RFC 3339 time: {value}") from error
    if parsed.tzinfo is None:
        raise ValueError(f"time must include a timezone: {value}")
    return parsed.astimezone(dt.timezone.utc)


def parse_duration(value):
    match = DURATION_RE.fullmatch(value)
    if not match:
        raise ValueError("duration must be a number followed by s, m, h, d, or w")
    return dt.timedelta(seconds=int(match.group(1)) * DURATION_UNITS[match.group(2)])


def rfc3339(value):
    return value.replace(microsecond=0).isoformat().replace("+00:00", "Z")


def choose_client(requested):
    if requested:
        if not shutil.which(requested):
            raise RuntimeError(f"--client {requested} was not found")
        return requested
    for client in ("oc", "kubectl"):
        if shutil.which(client):
            return client
    raise RuntimeError("port-forward requires oc or kubectl")


def run_json(command):
    completed = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if completed.returncode:
        return None
    return json.loads(completed.stdout)


def service_port(service):
    ports = service.get("spec", {}).get("ports", [])
    if not ports or "port" not in ports[0]:
        raise RuntimeError("discovered service has no port")
    return valid_port(str(ports[0]["port"]))


def discover_service(client, namespace):
    service = run_json([client, "--namespace", namespace, "get", "service", "thanos-querier", "-o", "json"])
    if service:
        return namespace, "svc/thanos-querier", service_port(service)

    services = run_json([client, "get", "service", "--all-namespaces", "-o", "json"])
    for item in (services or {}).get("items", []):
        if item.get("metadata", {}).get("name") == "thanos-querier":
            return item["metadata"]["namespace"], "svc/thanos-querier", service_port(item)
    raise RuntimeError("could not discover a thanos-querier Service; supply --port-forward RESOURCE or --server URL")


def oc_token(client):
    if client != "oc":
        return None
    completed = subprocess.run([client, "whoami", "-t"], text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    return completed.stdout.strip() if completed.returncode == 0 else None


def ssl_context(args, port_forward):
    if args.insecure or (port_forward and args.port_forward_scheme == "https"):
        return ssl._create_unverified_context()
    if args.ca_file:
        return ssl.create_default_context(cafile=args.ca_file)
    return ssl.create_default_context()


def request(url, token, context):
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers), context=context, timeout=30)


def wait_for_port_forward(process, url, token, context):
    for _ in range(30):
        try:
            with request(url + "/-/ready", token, context):
                return
        except urllib.error.HTTPError:
            return  # A HTTP response proves the local forward is connected.
        except urllib.error.URLError:
            if process.poll() is not None:
                break
            time.sleep(1)
    raise RuntimeError("port-forward did not become reachable")


def openmetrics(payload):
    if payload.get("status") != "success":
        raise RuntimeError(f"Prometheus API response status is {payload.get('status', 'missing')}")
    if payload.get("data", {}).get("resultType") != "matrix":
        raise RuntimeError("expected a Prometheus query_range matrix response")

    def family(name):
        if name.endswith(("_bucket", "_sum", "_count")):
            return name.rsplit("_", 1)[0]
        if name.endswith("_total"):
            return name[:-6]
        return name

    def metric_type(name):
        if name.endswith(("_bucket", "_sum", "_count")):
            return "histogram"
        return "counter" if name.endswith("_total") else "gauge"

    grouped = {}
    for series in payload["data"]["result"]:
        name = series["metric"]["__name__"]
        grouped.setdefault(family(name), []).append(series)

    lines = []
    for family_name in sorted(grouped):
        series_group = grouped[family_name]
        lines.append(f"# TYPE {family_name} {metric_type(series_group[0]['metric']['__name__'])}")
        for series in series_group:
            name = series["metric"]["__name__"]
            labels = ",".join(
                f"{key}={json.dumps(value)}" for key, value in sorted(series["metric"].items()) if key != "__name__"
            )
            label_text = f"{{{labels}}}" if labels else ""
            for timestamp, value in series["values"]:
                lines.append(f"{name}{label_text} {value} {int(float(timestamp) * 1000)}")
    return ("\n".join(lines) + "\n# EOF\n").encode()


def write_output(data, args, timestamp):
    extension = "om" if args.output_format == "openmetrics" else "json"
    output = args.output or f"kme-metrics-{timestamp:%Y%m%dT%H%M%SZ}.{extension}" + (".gz" if args.gzip else "")
    if output == "-":
        stream = sys.stdout.buffer
        with (gzip.GzipFile(fileobj=stream, mode="wb") if args.gzip else _no_close(stream)) as writer:
            writer.write(data)
        print("Wrote export to standard output", file=sys.stderr)
        return

    destination = Path(output)
    if destination.exists():
        raise RuntimeError(f"refusing to overwrite existing file: {destination}")
    with tempfile.NamedTemporaryFile(dir=destination.parent or Path("."), delete=False) as temporary:
        temporary_path = Path(temporary.name)
        try:
            if args.gzip:
                with gzip.GzipFile(fileobj=temporary, mode="wb") as writer:
                    writer.write(data)
            else:
                temporary.write(data)
            temporary_path.replace(destination)
        except Exception:
            temporary_path.unlink(missing_ok=True)
            raise
    print(f"Wrote {destination}", file=sys.stderr)


class _no_close:
    def __init__(self, stream): self.stream = stream
    def __enter__(self): return self.stream
    def __exit__(self, *args): return False


def main():
    args = parse_args()
    try:
        end = parse_time(args.end)
        start = parse_time(args.start) if args.start else end - parse_duration(args.duration)
        if start > end:
            raise ValueError("start time must not be after end time")

        requested_forward = args.port_forward is not None
        port_forward = not args.no_port_forward and (requested_forward or not args.server)
        client = process = log_file = None
        resource = args.port_forward
        namespace = args.port_forward_namespace
        token = args.bearer_token
        try:
            if port_forward:
                client = choose_client(args.client)
                if resource is None or resource == "":
                    namespace, resource, discovered_port = discover_service(client, namespace)
                else:
                    discovered_port = args.local_port
                remote_port = args.remote_port or discovered_port
                server = f"{args.port_forward_scheme}://127.0.0.1:{args.local_port}"
                token = token or oc_token(client)
                log_file = tempfile.NamedTemporaryFile(prefix="kme-port-forward-", delete=False)
                print(f"Starting {client} port-forward for {resource} in namespace {namespace}", file=sys.stderr)
                process = subprocess.Popen(
                    [client, "--namespace", namespace, "port-forward", resource, f"{args.local_port}:{remote_port}"],
                    stdout=log_file, stderr=subprocess.STDOUT,
                )
            else:
                server = args.server or "http://localhost:9090"

            context = ssl_context(args, port_forward)
            if process:
                wait_for_port_forward(process, server, token, context)
            selector = f'{{__name__=~"{METRIC_NAME_MATCHER}"'
            if args.vmi_namespace:
                selector += f',namespace={json.dumps(args.vmi_namespace)}'
            if args.vmi_name:
                selector += f',name={json.dumps(args.vmi_name)}'
            selector += "}"
            query = urllib.parse.urlencode({"query": selector, "start": rfc3339(start), "end": rfc3339(end), "step": args.step})
            print(f"Exporting KME metrics from {rfc3339(start)} through {rfc3339(end)} (step {args.step})", file=sys.stderr)
            with request(server.rstrip("/") + "/api/v1/query_range?" + query, token, context) as response:
                raw_data = response.read()
            data = openmetrics(json.loads(raw_data)) if args.output_format == "openmetrics" else raw_data
            write_output(data, args, end)
        finally:
            if process and process.poll() is None:
                process.terminate()
                process.wait(timeout=5)
            if log_file:
                log_file.close()
                Path(log_file.name).unlink(missing_ok=True)
    except (ValueError, RuntimeError, OSError, urllib.error.URLError, urllib.error.HTTPError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
