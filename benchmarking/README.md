# Substrate Benchmarking

This is the nascent suite for benchmarking Substrate's performance at scale.

## Deploy benchmarks

> [!IMPORTANT]
> Source the environment configuration file (e.g., `source .ate-dev-env.sh`)
> first so `PROJECT_ID`, `BUCKET_NAME`, etc. are set.

Note that deploying the benchmarks does not run them. You must visit Locust's
web UI to start a test.

A single wrapper deploys the scale workloads, builds and pushes the Locust
image, then deploys the Locust workers:

```bash
./benchmarking/deploy_locust.sh --deploy
```

Useful flags:

* `--worker-count N` — number of `WorkerPool` replicas (default 1).
* `--sandbox-class gvisor|microvm` — sandbox runtime for the benchmark
  `WorkerPool` (default `gvisor`). See [Benchmarking microVMs](#benchmarking-microvms).
* `--skip-build` — reuse the existing `:latest` locust image (skip the
  `docker build && docker push` step).

To tear everything down (locust then workloads, in reverse order):

```bash
./benchmarking/deploy_locust.sh --delete
```

The same operations are also reachable from the top-level installer for
convenience:

```bash
./hack/install-ate.sh --deploy-benchmarks
./hack/install-ate.sh --delete-benchmarks
```

The installer accepts `--benchmark-worker-count N` (default `1`).
`--skip-build` is only available when invoking
`benchmarking/deploy_locust.sh` directly.

## Running Tests

### Locust Web UI
* Run `kubectl port-forward svc/locust -n benchmarking 8089:8089`
* Visit `http://localhost:8089` in your browser to configure and start the load test.

The different user classes you can select are different types of load behaviors
you can throw at the system. Note that the "CounterUser" load type requires
that the counter demo be installed.

You can also configure things like the number of users, how quickly those users
are spawned, the frequency with which requests are made and whether or not tracing is
enabled.

### Viewing Traces
You must have enabled otel tracing for your cluster to view traces.

You can find trace IDs by viewing the `logs` tab in the Locust UI

## Benchmarking microVMs

Every benchmark runs on gVisor by default. To run the same load against
cloud-hypervisor microVMs instead, deploy with `--sandbox-class microvm`:

```bash
./benchmarking/deploy_locust.sh --deploy --sandbox-class microvm --worker-count 5
```

The nodes serving that pool need `/dev/kvm` and the
`ate.dev/sandboxClass=microvm` label; the WorkerPool carries a matching
nodeSelector and toleration, so pods stay `Pending` if no such node exists.

Both sandbox classes are covered in the automated suite. Tests in
`automation/tests.yaml` take an optional `sandboxClass`, and the microVM
entries there deliberately mirror a gVisor entry with the same users,
`workerCount`, and wait times, so a pair differs only in the runtime.

### Memory usage per sandbox

While a run is in flight, `kubectl ate top workers` reports CPU and memory for
the `ateom` container of every worker pod, alongside the actor currently
assigned to it:

```bash
kubectl ate top workers -n benchmark-workloads --sandbox-class microvm
```

This is the host's view: for a microVM it covers the whole sandbox — guest RAM,
the cloud-hypervisor process, and virtiofsd — which is the number that decides
how many actors fit on a node. It comes from metrics-server, so the pool needs
metrics-server installed and a sampling interval (~15s) to warm up.

### Putting the guest under memory pressure

At idle a glutton actor tells you the floor, not the cost of real work. The
`--glutton-ram-bytes` flag makes each actor hold a fixed number of bytes
resident, rewritten every iteration:

```bash
# 256MiB per actor, held across the suspend/resume cycle.
locust ... --glutton-ram-bytes 268435456
```

It is settable in the Locust web UI form, on the `runner.py` command line, and
per test in `automation/tests.yaml`. The boomer worker picks it up over
`/boomer-config` and issues `WriteRAM` to its actor between resume and ping, so
the ping latency is measured against a loaded guest and the suspend snapshot
that follows carries the same working set. `0` (the default) skips the write
entirely.

The value must fit in an `int32` (max `2147483647`), and it has to leave room
under the guest's RAM size — microVMs get 2048MiB by default
(`[hypervisor.clh] default_memory`).

## Optional: Prometheus + Grafana

Locust provides graphs, statistics, etc. via the UI. However, you
can install Prometheus/Grafana if you want richer details or
the ability to perform deeper analysis. Skip this section if
you're only using the Locust web UI.

```bash
kubectl apply -f benchmarking/monitoring.yaml
```

Once installed:

* Run `kubectl port-forward svc/grafana -n benchmarking 3000:3000`
* Visit `http://localhost:3000` in your browser.

## Development

### Rebuilding gRPC Python clients

Make sure you have a virtual environment created (`python3 -m venv venv`)
and activated (`source venv/bin/activate`).

Install project requirements: `pip install -r requirements.txt`

Then run `generate_protos.sh` to generate the Python proto clients.
